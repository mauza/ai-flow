package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func TestScheduleTriggerAndBacklogGuard(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["sandbox"].Spec.Schedules = []config.ProjectSchedule{{
		Name:         "daily-audit",
		Every:        flow.Duration{Duration: 24 * time.Hour},
		Task:         &config.ScheduleTask{Title: "Daily improvement audit", Body: "Audit repo"},
		MaxOpenTasks: 1,
	}}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New()}

	// First trigger creates task (plan fails gracefully without LLM, leaving task open or failed).
	st1, err := a.TriggerSchedule(ctx, "sandbox", "daily-audit", false)
	if err != nil {
		t.Fatal(err)
	}
	if st1.LastStatus != "started" || st1.LastTaskID == "" {
		t.Fatalf("first trigger: %+v", st1)
	}
	// Wait for the background planning attempt (no planner configured) to finish so it doesn't race with subsequent updates/close.
	deadline := time.Now().Add(2 * time.Second)
	for a.Planning(st1.LastTaskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Put task in open state and verify second trigger is skipped by max_open_tasks=1.
	_ = st.UpdateTask(ctx, st1.LastTaskID, map[string]any{"status": store.TaskFlowReady})
	st2, err := a.TriggerSchedule(ctx, "sandbox", "daily-audit", false)
	if err != nil {
		t.Fatal(err)
	}
	if st2.LastStatus != "skipped" {
		t.Fatalf("expected skipped due to max_open_tasks, got %+v", st2)
	}
	views := a.ListSchedules(ctx, "sandbox")
	if len(views) != 1 || views[0].Name != "daily-audit" || views[0].State.LastStatus != "skipped" {
		t.Fatalf("unexpected views: %+v", views)
	}

	// Dedup test for CreateTaskFromFlow.
	id1, c1, err := a.CreateTaskFromFlow(ctx, "sandbox", "flow", "fix-bug-1", "Fix bug 1", "body", false)
	if err != nil || !c1 || id1 == "" {
		t.Fatalf("first create: %s %v %v", id1, c1, err)
	}
	id2, c2, err := a.CreateTaskFromFlow(ctx, "sandbox", "flow", "fix-bug-1", "Fix bug 1", "body", false)
	if err != nil || c2 || id2 != id1 {
		t.Fatalf("duplicate should skip: %s %v %v", id2, c2, err)
	}
}
