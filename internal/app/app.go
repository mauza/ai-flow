// Package app holds the use cases shared by the HTTP API and intake sources:
// creating tasks, planning flows, saving versions, starting runs.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/planner"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// Notifier mirrors task progress to the task's source (Linear).
type Notifier interface {
	PlanStarted(ctx context.Context, t *store.Task)
	PlanFinished(ctx context.Context, t *store.Task, flowName string, valid bool, explanation string)
}

type App struct {
	Cfg     *config.Config
	Store   *store.Store
	Engine  *engine.Engine
	Planner *planner.Planner
	Hub     *hub.Hub

	notifiers []Notifier
	planning  sync.Map // task id → struct{}
}

func (a *App) AddNotifier(n Notifier) { a.notifiers = append(a.notifiers, n) }

// CreateTask stores a task and (optionally) starts planning it.
func (a *App) CreateTask(ctx context.Context, t *store.Task, plan bool) error {
	if t.ID == "" {
		t.ID = ids.New("t")
	}
	if t.Project == "" {
		for name := range a.Cfg.Projects {
			if t.Project == "" || name < t.Project {
				t.Project = name
			}
		}
	}
	if a.Cfg.Projects[t.Project] == nil {
		return fmt.Errorf("unknown project %q", t.Project)
	}
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("a task needs a title")
	}
	if err := a.Store.CreateTask(ctx, t); err != nil {
		return err
	}
	a.Store.AddEvent(ctx, &store.Event{TaskID: t.ID, Kind: "task", Message: "Task created: " + t.Title})
	a.Hub.Publish(hub.Event{Type: "task", ID: t.ID})
	if plan {
		a.PlanTask(t.ID)
	}
	return nil
}

// PlanTask drafts a flow for a task in the background.
func (a *App) PlanTask(taskID string) {
	if _, busy := a.planning.LoadOrStore(taskID, struct{}{}); busy {
		return
	}
	go func() {
		defer func() {
			a.planning.Delete(taskID)
			a.Hub.Publish(hub.Event{Type: "task", ID: taskID}) // clients drop the "planning" badge
		}()
		ctx := context.Background()
		if err := a.planTask(ctx, taskID); err != nil {
			slog.Error("planning failed", "task", taskID, "err", err)
			a.Store.UpdateTask(ctx, taskID, map[string]any{"status": store.TaskPlanFailed, "error": err.Error()})
			a.Store.AddEvent(ctx, &store.Event{TaskID: taskID, Kind: "plan", Message: "Planning failed: " + err.Error()})
			a.Hub.Publish(hub.Event{Type: "task", ID: taskID})
			if t, _ := a.Store.GetTask(ctx, taskID); t != nil {
				for _, n := range a.notifiers {
					n.PlanFinished(ctx, t, "", false, err.Error())
				}
			}
		}
	}()
}

// Planning reports whether a task is being planned right now.
func (a *App) Planning(taskID string) bool {
	_, ok := a.planning.Load(taskID)
	return ok
}

func (a *App) planTask(ctx context.Context, taskID string) error {
	t, err := a.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	name := t.FlowName
	if name == "" {
		name, err = a.uniqueFlowName(ctx, t)
		if err != nil {
			return err
		}
	}
	a.Store.UpdateTask(ctx, t.ID, map[string]any{"status": store.TaskPlanning, "flow_name": name, "error": ""})
	a.Store.AddEvent(ctx, &store.Event{TaskID: t.ID, FlowName: name, Kind: "plan", Message: "Planning started"})
	a.Hub.Publish(hub.Event{Type: "task", ID: t.ID})
	for _, n := range a.notifiers {
		n.PlanStarted(ctx, t)
	}

	req := planner.Request{FlowName: name, Project: t.Project, Title: t.Title, Body: t.Body, TaskRef: &flow.TaskRef{Source: t.Source, ID: firstNonEmpty(t.Identifier, t.ID)}}
	res, err := a.Planner.Plan(ctx, req)
	if err != nil {
		return err
	}
	if res.YAML == "" {
		return fmt.Errorf("the planner did not produce a flow after %d attempts", res.Attempts)
	}
	fv := &store.FlowVersion{Name: name, YAML: res.YAML, Project: t.Project, TaskID: t.ID, CreatedBy: "planner", Note: "Planned from task"}
	if _, err := a.Store.SaveFlow(ctx, fv); err != nil {
		return err
	}
	a.Store.AddChat(ctx, &store.ChatMessage{FlowName: name, Role: "user", Content: "Plan this task: " + t.Title})
	a.Store.AddChat(ctx, &store.ChatMessage{FlowName: name, Role: "assistant", Content: planSummary(res), YAML: res.YAML})
	status, msg := store.TaskFlowReady, fmt.Sprintf("Flow %s v%d ready", name, fv.Version)
	errText := ""
	if !res.Valid {
		status, msg = store.TaskPlanFailed, fmt.Sprintf("Flow %s v%d saved with validation errors; fix it in the editor", name, fv.Version)
		errText = issuesText(res.Issues)
	}
	a.Store.UpdateTask(ctx, t.ID, map[string]any{"status": status, "error": errText})
	a.Store.AddEvent(ctx, &store.Event{TaskID: t.ID, FlowName: name, Kind: "plan", Message: msg})
	a.Hub.Publish(hub.Event{Type: "task", ID: t.ID})
	a.Hub.Publish(hub.Event{Type: "flow", ID: name})
	t.FlowName = name
	for _, n := range a.notifiers {
		n.PlanFinished(ctx, t, name, res.Valid, res.Explanation)
	}
	if res.Valid && a.startMode(res.YAML, t.Project) == "auto" {
		if _, err := a.Engine.CreateRun(ctx, name, fv.Version, t.ID); err != nil {
			return fmt.Errorf("auto-start: %w", err)
		}
	}
	return nil
}

func planSummary(res *planner.Result) string {
	s := strings.TrimSpace(res.Explanation)
	if s == "" {
		s = "Here is a flow for this task."
	}
	if !res.Valid {
		s += "\n\nThe flow still has validation errors:\n" + issuesText(res.Issues)
	}
	return s
}

func issuesText(issues []resolve.Issue) string {
	var lines []string
	for _, i := range issues {
		if i.Severity == resolve.Error {
			lines = append(lines, "- "+i.String())
		}
	}
	return strings.Join(lines, "\n")
}

func (a *App) startMode(src, project string) string {
	if f, err := flow.Parse([]byte(src)); err == nil && f.Metadata.Start != "" {
		return f.Metadata.Start
	}
	if p := a.Cfg.Projects[project]; p != nil {
		return p.Spec.Start
	}
	return "manual"
}

func (a *App) uniqueFlowName(ctx context.Context, t *store.Task) (string, error) {
	base := ids.Slug(firstNonEmpty(t.Identifier+"-"+t.Title, t.Title), 48)
	if t.Identifier == "" {
		base = ids.Slug(t.Title, 48)
	}
	name := base
	for i := 2; ; i++ {
		exists, err := a.Store.FlowExists(ctx, name)
		if err != nil {
			return "", err
		}
		if !exists {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// SaveFlow stores a new version after checking that the name matches.
func (a *App) SaveFlow(ctx context.Context, name, src, who, note string) (*store.FlowVersion, []resolve.Issue, error) {
	f, err := flow.Parse([]byte(src))
	if err != nil {
		return nil, nil, err
	}
	if f.Metadata.Name != name {
		return nil, nil, fmt.Errorf("metadata.name is %q; it must stay %q", f.Metadata.Name, name)
	}
	issues := resolve.Validate(resolve.Resolve(f, a.Cfg), a.Cfg)
	prev, err := a.Store.GetFlow(ctx, name, 0)
	taskID := ""
	if err == nil {
		taskID = prev.TaskID
		if strings.TrimSpace(prev.YAML) == strings.TrimSpace(src) {
			return prev, issues, nil
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, nil, err
	}
	fv := &store.FlowVersion{Name: name, YAML: src, Project: f.Metadata.Project, TaskID: taskID, CreatedBy: who, Note: note}
	if _, err := a.Store.SaveFlow(ctx, fv); err != nil {
		return nil, nil, err
	}
	if taskID != "" {
		st := store.TaskFlowReady
		if resolve.HasErrors(issues) {
			st = store.TaskPlanFailed
		}
		if t, _ := a.Store.GetTask(ctx, taskID); t != nil && (t.Status == store.TaskFlowReady || t.Status == store.TaskPlanFailed) {
			a.Store.UpdateTask(ctx, taskID, map[string]any{"status": st, "error": issuesText(issues)})
			a.Hub.Publish(hub.Event{Type: "task", ID: taskID})
		}
	}
	a.Store.AddEvent(ctx, &store.Event{FlowName: name, TaskID: taskID, Kind: "flow", Message: fmt.Sprintf("Saved v%d (%s)", fv.Version, firstNonEmpty(note, who))})
	a.Hub.Publish(hub.Event{Type: "flow", ID: name})
	return fv, issues, nil
}

// Revise asks the planner to change a flow; nothing is saved.
func (a *App) Revise(ctx context.Context, name, current, message string) (*planner.Result, error) {
	f, err := flow.Parse([]byte(current))
	if err != nil {
		// Still let the planner try to repair it.
		f = &flow.Flow{Metadata: flow.Metadata{Name: name}}
	}
	fv, _ := a.Store.GetFlow(ctx, name, 0)
	project := f.Metadata.Project
	title, body := name, f.Spec.Description
	var ref *flow.TaskRef
	if fv != nil {
		if project == "" {
			project = fv.Project
		}
		if fv.TaskID != "" {
			if t, err := a.Store.GetTask(ctx, fv.TaskID); err == nil {
				title, body = t.Title, t.Body
				ref = &flow.TaskRef{Source: t.Source, ID: firstNonEmpty(t.Identifier, t.ID)}
			}
		}
	}
	if f.Metadata.Task != nil {
		ref = f.Metadata.Task
	}
	history, _ := a.Store.Chat(ctx, name)
	var turns []planner.Turn
	for _, m := range history {
		if m.Role == "user" || m.Role == "assistant" {
			turns = append(turns, planner.Turn{Role: m.Role, Content: m.Content})
		}
	}
	a.Store.AddChat(ctx, &store.ChatMessage{FlowName: name, Role: "user", Content: message})
	res, err := a.Planner.Revise(ctx, planner.Request{FlowName: name, Project: project, Title: title, Body: body, TaskRef: ref}, current, turns, message)
	if err != nil {
		a.Store.AddChat(ctx, &store.ChatMessage{FlowName: name, Role: "system", Content: "Planner error: " + err.Error()})
		return nil, err
	}
	a.Store.AddChat(ctx, &store.ChatMessage{FlowName: name, Role: "assistant", Content: planSummary(res), YAML: res.YAML})
	a.Hub.Publish(hub.Event{Type: "flow", ID: name})
	return res, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.Trim(x, "- ") != "" {
			return x
		}
	}
	return ""
}
