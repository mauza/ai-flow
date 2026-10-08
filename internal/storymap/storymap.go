// Package storymap reads and writes user story maps kept in a project's
// repository:
//
//	product/user-story-maps/<map>/map.yaml          personas, the journey, releases, metrics
//	product/user-story-maps/<map>/tasks/<task>.yaml one user task per file
//
// The journey runs left to right as phases, each holding activities (the
// backbone); user tasks hang under an activity and sit in a release slice.
// A user task, an activity or a whole phase can be handed to a flow.
package storymap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const Root = "product/user-story-maps"

type Map struct {
	Title       string    `yaml:"title" json:"title"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Personas    []Persona `yaml:"personas,omitempty" json:"personas"`
	Journey     []Phase   `yaml:"journey" json:"journey"`
	Releases    []Release `yaml:"releases,omitempty" json:"releases"`
	Metrics     []Metric  `yaml:"metrics,omitempty" json:"metrics"`
}

type Persona struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Phase is a stage of the user journey.
type Phase struct {
	ID          string     `yaml:"id" json:"id"`
	Title       string     `yaml:"title" json:"title"`
	Description string     `yaml:"description,omitempty" json:"description,omitempty"`
	Activities  []Activity `yaml:"activities" json:"activities"`
}

// Activity is a big thing a user does: a column of the backbone.
type Activity struct {
	ID          string `yaml:"id" json:"id"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Persona     string `yaml:"persona,omitempty" json:"persona,omitempty"`
}

// Release is a horizontal slice of the map, top to bottom.
type Release struct {
	ID     string `yaml:"id" json:"id"`
	Title  string `yaml:"title" json:"title"`
	Goal   string `yaml:"goal,omitempty" json:"goal,omitempty"`
	Status string `yaml:"status,omitempty" json:"status,omitempty"` // planned | in_progress | released
	Date   string `yaml:"date,omitempty" json:"date,omitempty"`
}

// Metric is tracked while the map's work ships. Product metrics are PromQL
// queries against the metrics backend; delivery metrics are computed by
// ai-flow from the tasks and their runs.
type Metric struct {
	ID          string   `yaml:"id" json:"id"`
	Title       string   `yaml:"title" json:"title"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Kind        string   `yaml:"kind" json:"kind"`                           // product | delivery
	Query       string   `yaml:"query,omitempty" json:"query,omitempty"`     // product
	Measure     string   `yaml:"measure,omitempty" json:"measure,omitempty"` // delivery: see Measures
	Release     string   `yaml:"release,omitempty" json:"release,omitempty"` // delivery: limit to one release
	Target      *float64 `yaml:"target,omitempty" json:"target,omitempty"`
	Direction   string   `yaml:"direction,omitempty" json:"direction,omitempty"` // up | down: which way is better
	Unit        string   `yaml:"unit,omitempty" json:"unit,omitempty"`
}

// Measures are the delivery metrics ai-flow computes.
var Measures = []string{"tasks_done", "done_ratio", "cycle_time_hours", "run_cost_usd", "run_success_rate"}

// Statuses a user task can have in the repository (set on purpose and committed).
var Statuses = []string{"todo", "in_progress", "done", "blocked"}

// Task is a user task: a card on the map.
type Task struct {
	ID          string   `yaml:"-" json:"id"`
	Title       string   `yaml:"title" json:"title"`
	Activity    string   `yaml:"activity" json:"activity"`
	Release     string   `yaml:"release,omitempty" json:"release,omitempty"`
	Order       int      `yaml:"order,omitempty" json:"order,omitempty"`
	Persona     string   `yaml:"persona,omitempty" json:"persona,omitempty"`
	Story       string   `yaml:"story,omitempty" json:"story,omitempty"` // As a … I want … so that …
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Acceptance  []string `yaml:"acceptance,omitempty" json:"acceptance,omitempty"`
	Metrics     []string `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	Status      string   `yaml:"status,omitempty" json:"status,omitempty"`
}

// Loaded is a map with its tasks, read from a workspace.
type Loaded struct {
	ID       string   `json:"id"`
	Map      Map      `json:"map"`
	Tasks    []Task   `json:"tasks"`
	Problems []string `json:"problems"`
}

// Summary describes a map in a listing.
type Summary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Tasks       int    `json:"tasks"`
	Done        int    `json:"done"`
	Error       string `json:"error,omitempty"`
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// CheckID accepts lowercase slugs (they become file and directory names).
func CheckID(kind, id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("%s id %q must be a lowercase slug (a-z, 0-9, -)", kind, id)
	}
	return nil
}

// Dir is a map's directory, relative to the repository root.
func Dir(mapID string) string { return path.Join(Root, mapID) }

// MapPath and TaskPath are file paths relative to the repository root.
func MapPath(mapID string) string          { return path.Join(Root, mapID, "map.yaml") }
func TaskPath(mapID, taskID string) string { return path.Join(Root, mapID, "tasks", taskID+".yaml") }

// List reads every map's summary from a workspace's work directory.
func List(work string) ([]Summary, error) {
	entries, err := os.ReadDir(filepath.Join(work, filepath.FromSlash(Root)))
	if errors.Is(err, fs.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		l, err := Load(work, e.Name())
		if err != nil {
			out = append(out, Summary{ID: e.Name(), Title: e.Name(), Error: err.Error()})
			continue
		}
		s := Summary{ID: l.ID, Title: l.Map.Title, Description: l.Map.Description, Tasks: len(l.Tasks)}
		for _, t := range l.Tasks {
			if t.Status == "done" {
				s.Done++
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Load reads a map and its tasks. Broken task files and dangling references
// are reported as problems so the rest of the map stays usable.
func Load(work, mapID string) (*Loaded, error) {
	if err := CheckID("map", mapID); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(MapPath(mapID))))
	if err != nil {
		return nil, err
	}
	l := &Loaded{ID: mapID, Tasks: []Task{}, Problems: []string{}}
	if err := decode(b, &l.Map); err != nil {
		return nil, fmt.Errorf("%s: %w", MapPath(mapID), err)
	}
	normalize(&l.Map)
	l.Problems = append(l.Problems, l.Map.check()...)
	files, _ := filepath.Glob(filepath.Join(work, filepath.FromSlash(Dir(mapID)), "tasks", "*.yaml"))
	sort.Strings(files)
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".yaml")
		rel := TaskPath(mapID, id)
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var t Task
		if err := decode(b, &t); err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		t.ID = id
		if err := CheckID("task", id); err != nil {
			l.Problems = append(l.Problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		l.Problems = append(l.Problems, l.Map.checkTask(&t)...)
		l.Tasks = append(l.Tasks, t)
	}
	sort.SliceStable(l.Tasks, func(i, j int) bool { return l.Tasks[i].Order < l.Tasks[j].Order })
	return l, nil
}

func decode(b []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func normalize(m *Map) {
	for _, s := range []*[]Persona{&m.Personas} {
		if *s == nil {
			*s = []Persona{}
		}
	}
	if m.Journey == nil {
		m.Journey = []Phase{}
	}
	for i := range m.Journey {
		if m.Journey[i].Activities == nil {
			m.Journey[i].Activities = []Activity{}
		}
	}
	if m.Releases == nil {
		m.Releases = []Release{}
	}
	if m.Metrics == nil {
		m.Metrics = []Metric{}
	}
}

// Check validates a map document before it is written.
func (m *Map) Check() error {
	if problems := m.check(); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (m *Map) check() []string {
	var out []string
	if strings.TrimSpace(m.Title) == "" {
		out = append(out, "map: title is required")
	}
	seen := map[string]bool{}
	unique := func(kind, id string) {
		if err := CheckID(kind, id); err != nil {
			out = append(out, err.Error())
			return
		}
		if seen[kind+"/"+id] {
			out = append(out, fmt.Sprintf("%s id %q is used twice", kind, id))
		}
		seen[kind+"/"+id] = true
	}
	for _, p := range m.Personas {
		unique("persona", p.ID)
	}
	for _, ph := range m.Journey {
		unique("phase", ph.ID)
		for _, a := range ph.Activities {
			unique("activity", a.ID)
			if a.Persona != "" && !slices.ContainsFunc(m.Personas, func(p Persona) bool { return p.ID == a.Persona }) {
				out = append(out, fmt.Sprintf("activity %s: unknown persona %q", a.ID, a.Persona))
			}
		}
	}
	for _, r := range m.Releases {
		unique("release", r.ID)
		if r.Status != "" && !slices.Contains([]string{"planned", "in_progress", "released"}, r.Status) {
			out = append(out, fmt.Sprintf("release %s: status must be planned, in_progress or released", r.ID))
		}
	}
	for _, mt := range m.Metrics {
		unique("metric", mt.ID)
		switch mt.Kind {
		case "product":
			if mt.Query == "" {
				out = append(out, fmt.Sprintf("metric %s: a product metric needs a PromQL query", mt.ID))
			}
		case "delivery":
			if !slices.Contains(Measures, mt.Measure) {
				out = append(out, fmt.Sprintf("metric %s: measure must be one of %s", mt.ID, strings.Join(Measures, ", ")))
			}
		default:
			out = append(out, fmt.Sprintf("metric %s: kind must be product or delivery", mt.ID))
		}
		if mt.Release != "" && m.Release(mt.Release) == nil {
			out = append(out, fmt.Sprintf("metric %s: unknown release %q", mt.ID, mt.Release))
		}
		if mt.Direction != "" && mt.Direction != "up" && mt.Direction != "down" {
			out = append(out, fmt.Sprintf("metric %s: direction must be up or down", mt.ID))
		}
	}
	return out
}

// CheckTask validates a task before it is written: structure is required,
// references must point at things in the map.
func (m *Map) CheckTask(t *Task) error {
	if err := CheckID("task", t.ID); err != nil {
		return err
	}
	if problems := m.checkTask(t); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (m *Map) checkTask(t *Task) []string {
	var out []string
	where := "task " + t.ID
	if strings.TrimSpace(t.Title) == "" {
		out = append(out, where+": title is required")
	}
	if _, a := m.Activity(t.Activity); a == nil {
		out = append(out, fmt.Sprintf("%s: unknown activity %q", where, t.Activity))
	}
	if t.Release != "" && m.Release(t.Release) == nil {
		out = append(out, fmt.Sprintf("%s: unknown release %q", where, t.Release))
	}
	if t.Persona != "" && !slices.ContainsFunc(m.Personas, func(p Persona) bool { return p.ID == t.Persona }) {
		out = append(out, fmt.Sprintf("%s: unknown persona %q", where, t.Persona))
	}
	for _, id := range t.Metrics {
		if !slices.ContainsFunc(m.Metrics, func(mt Metric) bool { return mt.ID == id }) {
			out = append(out, fmt.Sprintf("%s: unknown metric %q", where, id))
		}
	}
	if t.Status != "" && !slices.Contains(Statuses, t.Status) {
		out = append(out, fmt.Sprintf("%s: status must be one of %s", where, strings.Join(Statuses, ", ")))
	}
	return out
}

// Activity finds an activity and its phase.
func (m *Map) Activity(id string) (*Phase, *Activity) {
	for i := range m.Journey {
		for j := range m.Journey[i].Activities {
			if m.Journey[i].Activities[j].ID == id {
				return &m.Journey[i], &m.Journey[i].Activities[j]
			}
		}
	}
	return nil, nil
}

func (m *Map) Phase(id string) *Phase {
	for i := range m.Journey {
		if m.Journey[i].ID == id {
			return &m.Journey[i]
		}
	}
	return nil
}

func (m *Map) Release(id string) *Release {
	for i := range m.Releases {
		if m.Releases[i].ID == id {
			return &m.Releases[i]
		}
	}
	return nil
}

func (m *Map) Persona(id string) *Persona {
	for i := range m.Personas {
		if m.Personas[i].ID == id {
			return &m.Personas[i]
		}
	}
	return nil
}

// Marshal renders a map or task as YAML in field order.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}
