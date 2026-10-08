package storymap

import (
	"fmt"
	"strings"
)

// Scope is the part of a map handed to a flow: one user task, an activity or
// a phase (optionally limited to one release).
type Scope struct {
	Map     string   `json:"map"`
	Kind    string   `json:"kind"` // task | activity | phase
	ID      string   `json:"id"`
	Release string   `json:"release,omitempty"`
	Tasks   []string `json:"tasks"` // the user tasks it covers
}

// Select resolves a scope to its user tasks. Activities and phases skip
// tasks already marked done.
func (l *Loaded) Select(kind, id, release string) (*Scope, []Task, error) {
	if release != "" && l.Map.Release(release) == nil {
		return nil, nil, fmt.Errorf("unknown release %q", release)
	}
	var in func(t *Task) bool
	switch kind {
	case "task":
		in = func(t *Task) bool { return t.ID == id }
	case "activity":
		if _, a := l.Map.Activity(id); a == nil {
			return nil, nil, fmt.Errorf("unknown activity %q", id)
		}
		in = func(t *Task) bool { return t.Activity == id }
	case "phase":
		ph := l.Map.Phase(id)
		if ph == nil {
			return nil, nil, fmt.Errorf("unknown phase %q", id)
		}
		acts := map[string]bool{}
		for _, a := range ph.Activities {
			acts[a.ID] = true
		}
		in = func(t *Task) bool { return acts[t.Activity] }
	default:
		return nil, nil, fmt.Errorf("kind must be task, activity or phase")
	}
	sc := &Scope{Map: l.ID, Kind: kind, ID: id, Release: release, Tasks: []string{}}
	candidates := l.orderedTasks()
	if kind == "task" {
		candidates = l.Tasks // a single task even if it is not placed on the map yet
	}
	var tasks []Task
	for _, t := range candidates {
		if !in(&t) || (kind != "task" && (t.Status == "done" || (release != "" && t.Release != release))) {
			continue
		}
		sc.Tasks = append(sc.Tasks, t.ID)
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		if kind == "task" {
			return nil, nil, fmt.Errorf("unknown user task %q", id)
		}
		return nil, nil, fmt.Errorf("%s %s has no open user tasks%s", kind, id, map[bool]string{true: " in release " + release}[release != ""])
	}
	return sc, tasks, nil
}

// orderedTasks walks the map in reading order: journey left to right, then
// releases top to bottom, then card order.
func (l *Loaded) orderedTasks() []Task {
	var out []Task
	releases := append([]string{}, "")
	for _, r := range l.Map.Releases {
		releases = append(releases, r.ID)
	}
	for _, ph := range l.Map.Journey {
		for _, a := range ph.Activities {
			for _, r := range releases {
				for _, t := range l.Tasks {
					if t.Activity == a.ID && t.Release == r {
						out = append(out, t)
					}
				}
			}
		}
	}
	return out
}

// Title names the work for a scope.
func (l *Loaded) Title(sc *Scope, tasks []Task) string {
	switch sc.Kind {
	case "task":
		return tasks[0].Title
	case "activity":
		_, a := l.Map.Activity(sc.ID)
		return fmt.Sprintf("%s: %s (%d user tasks)", l.Map.Title, a.Title, len(tasks))
	default:
		return fmt.Sprintf("%s: %s phase (%d user tasks)", l.Map.Title, l.Map.Phase(sc.ID).Title, len(tasks))
	}
}

// Brief is the task description the planner and every step see: the user
// tasks with their stories and acceptance criteria, in the context of the
// map, plus where the product docs live.
func (l *Loaded) Brief(sc *Scope, tasks []Task) string {
	var b strings.Builder
	m := &l.Map
	fmt.Fprintf(&b, "This work comes from the user story map **%s** (`%s/`).\n", m.Title, Dir(l.ID))
	if m.Description != "" {
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(m.Description))
	}
	b.WriteString("Product documentation is in `product/` of this repository; read `product/README.md` and the map for context. ")
	b.WriteString("Do not edit files under `product/`: the product owner changes those.\n")
	switch sc.Kind {
	case "activity":
		ph, a := m.Activity(sc.ID)
		fmt.Fprintf(&b, "\nScope: the **%s** activity in the %s phase", a.Title, ph.Title)
	case "phase":
		fmt.Fprintf(&b, "\nScope: the **%s** phase of the journey", m.Phase(sc.ID).Title)
	}
	if sc.Kind != "task" {
		if sc.Release != "" {
			fmt.Fprintf(&b, ", release %s", m.Release(sc.Release).Title)
		}
		b.WriteString(". Deliver the user tasks below together, in order.\n")
	}
	for _, t := range tasks {
		b.WriteString("\n")
		writeTask(&b, m, &t, sc.Kind != "task")
	}
	return b.String()
}

func writeTask(b *strings.Builder, m *Map, t *Task, heading bool) {
	if heading {
		fmt.Fprintf(b, "## User task: %s (`%s`)\n", t.Title, t.ID)
	} else {
		fmt.Fprintf(b, "## User task `%s`\n", t.ID)
	}
	if ph, a := m.Activity(t.Activity); a != nil {
		fmt.Fprintf(b, "Journey: %s → %s\n", ph.Title, a.Title)
	}
	persona := t.Persona
	if persona == "" {
		if _, a := m.Activity(t.Activity); a != nil {
			persona = a.Persona
		}
	}
	if p := m.Persona(persona); p != nil {
		fmt.Fprintf(b, "Persona: %s", p.Name)
		if p.Description != "" {
			fmt.Fprintf(b, " (%s)", strings.TrimSpace(p.Description))
		}
		b.WriteString("\n")
	}
	if r := m.Release(t.Release); r != nil {
		fmt.Fprintf(b, "Release: %s", r.Title)
		if r.Goal != "" {
			fmt.Fprintf(b, " (goal: %s)", strings.TrimSpace(r.Goal))
		}
		b.WriteString("\n")
	}
	if t.Story != "" {
		fmt.Fprintf(b, "\nStory: %s\n", strings.TrimSpace(t.Story))
	}
	if t.Description != "" {
		fmt.Fprintf(b, "\n%s\n", strings.TrimSpace(t.Description))
	}
	if len(t.Acceptance) > 0 {
		b.WriteString("\nAcceptance criteria:\n")
		for _, a := range t.Acceptance {
			fmt.Fprintf(b, "- %s\n", a)
		}
	}
	for _, id := range t.Metrics {
		for _, mt := range m.Metrics {
			if mt.ID == id {
				fmt.Fprintf(b, "Should move metric: %s", mt.Title)
				if mt.Target != nil {
					fmt.Fprintf(b, " (target %g%s)", *mt.Target, unitSuffix(mt.Unit))
				}
				b.WriteString("\n")
			}
		}
	}
}

func unitSuffix(u string) string {
	if u == "" {
		return ""
	}
	return " " + u
}
