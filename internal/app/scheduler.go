package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/ids"
	"github.com/mauza/ai-flow/internal/store"
)

// ScheduleState tracks the last execution of one project schedule in KV.
type ScheduleState struct {
	LastRunAt  int64  `json:"last_run_at,omitempty"`
	LastStatus string `json:"last_status,omitempty"` // started | skipped | error
	LastReason string `json:"last_reason,omitempty"`
	LastRunID  string `json:"last_run_id,omitempty"`
	LastTaskID string `json:"last_task_id,omitempty"`
}

// ScheduleView combines a configured ProjectSchedule with its live runtime state.
type ScheduleView struct {
	Project       string        `json:"project"`
	Name          string        `json:"name"`
	Enabled       bool          `json:"enabled"`
	Every         string        `json:"every"`
	Flow          string        `json:"flow,omitempty"`
	TaskTitle     string        `json:"task_title,omitempty"`
	MaxActiveRuns int           `json:"max_active_runs"`
	MaxOpenTasks  int           `json:"max_open_tasks"`
	NextDueAt     int64         `json:"next_due_at,omitempty"`
	State         ScheduleState `json:"state"`
}

func scheduleKVKey(project, name string) string {
	return "schedule/" + project + "/" + name
}

func (a *App) loadScheduleState(ctx context.Context, project, name string) ScheduleState {
	var st ScheduleState
	if raw, err := a.Store.GetKV(ctx, scheduleKVKey(project, name)); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}

func (a *App) saveScheduleState(ctx context.Context, project, name string, st ScheduleState) {
	b, _ := json.Marshal(st)
	_ = a.Store.SetKV(ctx, scheduleKVKey(project, name), string(b))
}

// ListSchedules returns every configured schedule (optionally filtered to one
// project) together with its last execution state and next due timestamp.
func (a *App) ListSchedules(ctx context.Context, project string) []ScheduleView {
	cfg := a.Cfg.Current()
	out := []ScheduleView{}
	for _, pname := range slices.Sorted(maps.Keys(cfg.Projects)) {
		if project != "" && pname != project {
			continue
		}
		p := cfg.Projects[pname]
		for _, sc := range p.Spec.Schedules {
			st := a.loadScheduleState(ctx, pname, sc.Name)
			view := ScheduleView{
				Project:       pname,
				Name:          sc.Name,
				Enabled:       sc.IsEnabled(),
				Every:         sc.Every.Duration.String(),
				Flow:          sc.Flow,
				MaxActiveRuns: sc.EffectiveMaxActiveRuns(),
				MaxOpenTasks:  sc.EffectiveMaxOpenTasks(),
				State:         st,
			}
			if sc.Task != nil {
				view.TaskTitle = sc.Task.Title
			}
			if sc.IsEnabled() && sc.Every.Duration > 0 {
				if st.LastRunAt > 0 {
					view.NextDueAt = st.LastRunAt + sc.Every.Duration.Milliseconds()
				} else {
					view.NextDueAt = store.Now()
				}
			}
			out = append(out, view)
		}
	}
	return out
}

// TriggerSchedule runs one configured schedule immediately. When force is false,
// it honors max_active_runs and max_open_tasks guards.
func (a *App) TriggerSchedule(ctx context.Context, project, name string, force bool) (*ScheduleState, error) {
	cfg := a.Cfg.Current()
	p := cfg.Projects[project]
	if p == nil {
		return nil, fmt.Errorf("unknown project %q", project)
	}
	var sc *config.ProjectSchedule
	for i := range p.Spec.Schedules {
		if p.Spec.Schedules[i].Name == name {
			sc = &p.Spec.Schedules[i]
			break
		}
	}
	if sc == nil {
		return nil, fmt.Errorf("project %q has no schedule %q", project, name)
	}
	now := store.Now()
	st := ScheduleState{LastRunAt: now}
	if !force {
		if reason, err := a.scheduleBlockedReason(ctx, project, *sc); err != nil {
			return nil, err
		} else if reason != "" {
			st.LastStatus = "skipped"
			st.LastReason = reason
			a.saveScheduleState(ctx, project, name, st)
			return &st, nil
		}
	}
	if strings.TrimSpace(sc.Flow) != "" {
		if a.Engine == nil {
			return nil, fmt.Errorf("engine is not configured")
		}
		run, err := a.Engine.CreateRun(ctx, sc.Flow, 0, "")
		if err != nil {
			st.LastStatus = "error"
			st.LastReason = err.Error()
			a.saveScheduleState(ctx, project, name, st)
			return &st, err
		}
		st.LastStatus = "started"
		st.LastRunID = run.ID
		st.LastReason = fmt.Sprintf("Started run %s of %s", run.ID, sc.Flow)
		a.saveScheduleState(ctx, project, name, st)
		return &st, nil
	}
	if sc.Task == nil || strings.TrimSpace(sc.Task.Title) == "" {
		return nil, fmt.Errorf("schedule %q has neither flow nor task", name)
	}
	stamp := time.UnixMilli(now).UTC().Format("2006-01-02")
	t := &store.Task{
		ID:         ids.New("t"),
		Source:     "schedule",
		Identifier: fmt.Sprintf("%s-%s", sc.Name, stamp),
		Title:      sc.Task.Title,
		Body:       sc.Task.Body,
		Project:    project,
	}
	if err := a.CreateTask(ctx, t, true); err != nil {
		st.LastStatus = "error"
		st.LastReason = err.Error()
		a.saveScheduleState(ctx, project, name, st)
		return &st, err
	}
	st.LastStatus = "started"
	st.LastTaskID = t.ID
	st.LastReason = fmt.Sprintf("Created scheduled task %s", t.ID)
	a.saveScheduleState(ctx, project, name, st)
	return &st, nil
}

func (a *App) scheduleBlockedReason(ctx context.Context, project string, sc config.ProjectSchedule) (string, error) {
	active, err := a.Store.ListRuns(ctx, store.RunFilter{
		Project:  project,
		Statuses: []string{store.RunQueued, store.RunRunning, store.RunWaiting},
		Limit:    100,
	})
	if err != nil {
		return "", err
	}
	if len(active) >= sc.EffectiveMaxActiveRuns() {
		return fmt.Sprintf("skipped: %d active run(s) already in project %s (max %d)", len(active), project, sc.EffectiveMaxActiveRuns()), nil
	}
	tasks, err := a.Store.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	open := 0
	for _, t := range tasks {
		if t.Project != project {
			continue
		}
		if t.Status == store.TaskNew || t.Status == store.TaskPlanning || t.Status == store.TaskFlowReady || t.Status == store.TaskRunning {
			open++
		}
	}
	if open >= sc.EffectiveMaxOpenTasks() {
		return fmt.Sprintf("skipped: %d open task(s) already in project %s (max %d)", open, project, sc.EffectiveMaxOpenTasks()), nil
	}
	return "", nil
}

// RunScheduleLoop periodically checks configured project schedules and triggers
// any that are due.
func (a *App) RunScheduleLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.tickSchedules(ctx)
		}
	}
}

func (a *App) tickSchedules(ctx context.Context) {
	cfg := a.Cfg.Current()
	now := store.Now()
	for _, pname := range slices.Sorted(maps.Keys(cfg.Projects)) {
		p := cfg.Projects[pname]
		for _, sc := range p.Spec.Schedules {
			if !sc.IsEnabled() || sc.Every.Duration <= 0 {
				continue
			}
			st := a.loadScheduleState(ctx, pname, sc.Name)
			if st.LastRunAt > 0 && now-st.LastRunAt < sc.Every.Duration.Milliseconds() {
				continue
			}
			res, err := a.TriggerSchedule(ctx, pname, sc.Name, false)
			if err != nil {
				slog.Warn("scheduled trigger failed", "project", pname, "schedule", sc.Name, "err", err)
			} else if res != nil && res.LastStatus == "started" {
				slog.Info("scheduled trigger started", "project", pname, "schedule", sc.Name, "run", res.LastRunID, "task", res.LastTaskID)
			}
		}
	}
}

// CreateTaskFromFlow creates a deduplicated child task spawned by a flow's
// create_tasks action. Duplicate (source, externalID) pairs return created=false
// with the existing task ID.
func (a *App) CreateTaskFromFlow(ctx context.Context, project, source, externalID, title, body string, plan bool) (taskID string, created bool, err error) {
	if source == "" {
		source = "flow"
	}
	if externalID != "" {
		if existing, err := a.Store.TaskByExternal(ctx, source, externalID); err == nil && existing != nil {
			return existing.ID, false, nil
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}
	t := &store.Task{
		ID:         ids.New("t"),
		Source:     source,
		ExternalID: externalID,
		Identifier: externalID,
		Title:      strings.TrimSpace(title),
		Body:       body,
		Project:    project,
	}
	if err := a.CreateTask(ctx, t, plan); err != nil {
		return "", false, err
	}
	return t.ID, true, nil
}
