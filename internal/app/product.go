package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/metrics"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
	"github.com/mauza/ai-flow/internal/workspace"
)

// SourceStoryMap marks ai-flow tasks created from a user story map.
const SourceStoryMap = "storymap"

// kvScope records which user tasks an ai-flow task covers:
// storymap/scope/<project>/<map>/<task id> → storymap.Scope as JSON.
func kvScope(project, mapID, taskID string) string {
	return path.Join("storymap/scope", project, mapID, taskID)
}

// ProductSource is where a project's product workspace comes from: its repo
// grant on GitHub and its base branch.
func (a *App) ProductSource(project string) (workspace.Source, error) {
	cfg := a.Cfg.Current()
	p := cfg.Projects[project]
	if p == nil {
		return workspace.Source{}, fmt.Errorf("unknown project %q", project)
	}
	g := cfg.Catalog.Grants[p.Spec.Repo]
	if g == nil || g.Kind != config.GrantGit {
		return workspace.Source{}, fmt.Errorf("project %s: repo %q is not a git grant", project, p.Spec.Repo)
	}
	repo, err := github.ParseRepoURL(g.URL)
	if err != nil {
		return workspace.Source{}, err
	}
	if repo.Host != "github.com" {
		return workspace.Source{}, fmt.Errorf("project %s: product workspaces need a GitHub repository, not %s", project, repo.Host)
	}
	return workspace.Source{Repo: repo, Branch: p.Spec.Base}, nil
}

// SendToFlow hands part of a story map to ai-flow: it creates a task whose
// description is the scope's brief and (optionally) starts planning it.
func (a *App) SendToFlow(ctx context.Context, project, mapID, kind, id, release string, plan bool) (*store.Task, error) {
	l, err := storymap.Load(a.Workspaces.WorkDir(project), mapID)
	if err != nil {
		return nil, err
	}
	sc, tasks, err := l.Select(kind, id, release)
	if err != nil {
		return nil, err
	}
	t := &store.Task{
		Source:     SourceStoryMap,
		ExternalID: fmt.Sprintf("%s/%s/%s/%s/%d", project, mapID, kind, id, store.Now()),
		Identifier: id,
		Title:      l.Title(sc, tasks),
		Body:       l.Brief(sc, tasks),
		Project:    project,
	}
	// Record the scope first so the map can show the task as soon as it exists.
	t.ID = ids.New("t")
	b, _ := json.Marshal(sc)
	if err := a.Store.SetKV(ctx, kvScope(project, mapID, t.ID), string(b)); err != nil {
		return nil, err
	}
	if err := a.CreateTask(ctx, t, plan); err != nil {
		a.Store.WriteKV(ctx, nil, []string{kvScope(project, mapID, t.ID)})
		return nil, err
	}
	return t, nil
}

// MapWork is an ai-flow task created from a map, with its latest run.
type MapWork struct {
	Scope storymap.Scope `json:"scope"`
	Task  *store.Task    `json:"task"`
	Run   *store.Run     `json:"run,omitempty"`
	Runs  []*store.Run   `json:"-"`
}

// MapWorks lists the ai-flow tasks created from a map, newest first.
func (a *App) MapWorks(ctx context.Context, project, mapID string) ([]*MapWork, error) {
	scopes, err := a.Store.ListKV(ctx, kvScope(project, mapID, "")+"/")
	if err != nil {
		return nil, err
	}
	var out []*MapWork
	for key, raw := range scopes {
		var w MapWork
		if err := json.Unmarshal([]byte(raw), &w.Scope); err != nil {
			continue
		}
		t, err := a.Store.GetTask(ctx, path.Base(key))
		if errors.Is(err, store.ErrNotFound) {
			continue // the task was deleted
		}
		if err != nil {
			return nil, err
		}
		w.Task, _ = a.TaskState(t)
		w.Runs, _ = a.Store.ListRuns(ctx, store.RunFilter{TaskID: t.ID, Limit: 50})
		if len(w.Runs) > 0 {
			w.Run = w.Runs[0]
		}
		out = append(out, &w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task.CreatedAt > out[j].Task.CreatedAt })
	return out, nil
}

// MetricValue is a metric's current value (and recent history for product metrics).
type MetricValue struct {
	ID     string       `json:"id"`
	Value  *float64     `json:"value,omitempty"`
	Series [][2]float64 `json:"series,omitempty"` // [unix seconds, value]
	Detail string       `json:"detail,omitempty"`
	Error  string       `json:"error,omitempty"`
}

// MapMetrics evaluates a map's metrics: product metrics against the metrics
// backend (now and the last 7 days), delivery metrics from the map's tasks and
// the runs they started.
func (a *App) MapMetrics(ctx context.Context, project string, l *storymap.Loaded, works []*MapWork) []MetricValue {
	out := []MetricValue{}
	url := a.Cfg.Current().Env.Metrics.URL
	for _, m := range l.Map.Metrics {
		v := MetricValue{ID: m.ID}
		switch m.Kind {
		case "product":
			if url == "" {
				v.Error = "metrics.url is not configured"
				break
			}
			// A product metric is the sum over the query's series.
			values, err := metrics.Instant(ctx, nil, url, m.Query)
			if err != nil {
				v.Error = err.Error()
				break
			}
			if len(values) > 0 {
				sum := 0.0
				for _, x := range values {
					sum += x
				}
				v.Value = &sum
			}
			v.Series, _ = metrics.Range(ctx, nil, url, m.Query, 7*24*3600, 3600)
		case "delivery":
			v.Value, v.Detail = delivery(m, l, works)
		}
		out = append(out, v)
	}
	return out
}

func delivery(m storymap.Metric, l *storymap.Loaded, works []*MapWork) (*float64, string) {
	inRelease := map[string]bool{}
	total, done := 0, 0
	for _, t := range l.Tasks {
		if m.Release != "" && t.Release != m.Release {
			continue
		}
		inRelease[t.ID] = true
		total++
		if t.Status == "done" {
			done++
		}
	}
	// Work counts toward a release when it covers any of the release's tasks.
	var scoped []*MapWork
	for _, w := range works {
		for _, id := range w.Scope.Tasks {
			if inRelease[id] {
				scoped = append(scoped, w)
				break
			}
		}
	}
	val := func(f float64) *float64 { return &f }
	switch m.Measure {
	case "tasks_done":
		return val(float64(done)), fmt.Sprintf("%d of %d user tasks done", done, total)
	case "done_ratio":
		if total == 0 {
			return nil, "no user tasks"
		}
		return val(math.Round(1000*float64(done)/float64(total)) / 10), fmt.Sprintf("%d of %d user tasks done", done, total)
	case "cycle_time_hours":
		var hours []float64
		for _, w := range scoped {
			for _, r := range w.Runs {
				if r.Status == store.RunSucceeded && r.FinishedAt > 0 {
					hours = append(hours, float64(r.FinishedAt-w.Task.CreatedAt)/3.6e6)
					break
				}
			}
		}
		if len(hours) == 0 {
			return nil, "no succeeded runs yet"
		}
		sort.Float64s(hours)
		med := hours[len(hours)/2]
		if len(hours)%2 == 0 {
			med = (hours[len(hours)/2-1] + hours[len(hours)/2]) / 2
		}
		return val(math.Round(med*10) / 10), fmt.Sprintf("median from sending to a succeeded run, over %d", len(hours))
	case "run_cost_usd":
		sum, n := 0.0, 0
		for _, w := range scoped {
			for _, r := range w.Runs {
				sum += r.CostUSD
				n++
			}
		}
		return val(math.Round(sum*100) / 100), fmt.Sprintf("%d runs", n)
	case "run_success_rate":
		ok, finished := 0, 0
		for _, w := range scoped {
			for _, r := range w.Runs {
				if store.RunDone(r.Status) {
					finished++
					if r.Status == store.RunSucceeded {
						ok++
					}
				}
			}
		}
		if finished == 0 {
			return nil, "no finished runs yet"
		}
		return val(math.Round(1000*float64(ok)/float64(finished)) / 10), fmt.Sprintf("%d of %d finished runs succeeded", ok, finished)
	}
	return nil, "unknown measure"
}

// LinkRepo adds a GitHub repository as a project: a read/write git grant and
// a project that may use it (and every model). It returns the project name.
func (a *App) LinkRepo(ctx context.Context, fullName, name, description string) (string, error) {
	owner, repoName, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || repoName == "" {
		return "", fmt.Errorf("repository must be owner/name")
	}
	info, err := a.GitHub.Repository(ctx, github.Repo{Host: "github.com", Owner: owner, Name: repoName})
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", fullName, err)
	}
	if name == "" {
		name = strings.ToLower(repoName)
	}
	if err := storymap.CheckID("project", name); err != nil {
		return "", err
	}
	cfg := a.Cfg.Current()
	if cfg.Projects[name] != nil {
		return "", fmt.Errorf("project %s already exists", name)
	}
	url := "https://github.com/" + info.FullName + ".git"
	grant := "repo/" + name
	if g := cfg.Catalog.Grants[grant]; g != nil && !sameRepo(g.URL, url) {
		return "", fmt.Errorf("grant %s already points at %s", grant, g.URL)
	}
	if description == "" {
		description = info.Description
	}
	edits := []ConfigEdit{
		{Section: "grants", Name: grant, YAML: yamlDoc(map[string]any{
			"kind": "git", "url": url, "modes": []string{"read", "write"}, "description": "GitHub " + info.FullName,
		})},
		{Section: "projects", Name: name, YAML: yamlDoc(map[string]any{"spec": map[string]any{
			"description": description, "repo": grant, "base": info.DefaultBranch, "start": "manual",
			"allow": map[string]any{"grants": []string{grant + ":*"}},
		}})},
	}
	if _, err := a.EditConfig(ctx, false, edits...); err != nil {
		return "", err
	}
	return name, nil
}

func sameRepo(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")) }
	return norm(a) == norm(b)
}

func yamlDoc(v any) string {
	b, _ := json.Marshal(v) // JSON is YAML
	return string(b)
}
