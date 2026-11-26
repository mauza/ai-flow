package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/workspace"
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
		AutoStart:    true,
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
		t.Fatalf("a queued task should fill the only slot, got %+v", st2)
	}
	views := a.ListSchedules(ctx, "sandbox")
	if len(views) != 1 || views[0].Name != "daily-audit" || views[0].State.LastStatus != "skipped" {
		t.Fatalf("unexpected views: %+v", views)
	}
}

func TestStoryMapSchedulePicksNextReadyCardAndDedups(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["sandbox"].Spec.Schedules = []config.ProjectSchedule{{
		Name:         "storymap-runner",
		Every:        flow.Duration{Duration: 2 * time.Hour},
		StoryMap:     &config.ScheduleStoryMap{Map: "core", MaxPerTick: 1},
		AutoStart:    true,
		MaxOpenTasks: 5,
	}}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	wsRoot := t.TempDir()
	ws := workspace.New(wsRoot, nil, store.Now)
	mapDir := filepath.Join(ws.WorkDir("sandbox"), "product", "user-story-maps", "core")
	if err := os.MkdirAll(filepath.Join(mapDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	mapYAML := `title: Core Loop
journey:
  - id: discover
    title: Discover
    activities:
      - id: read
        title: Read
releases:
  - id: r0
    title: Shipped
    status: released
  - id: r1
    title: Next
    status: in_progress
`
	if err := os.WriteFile(filepath.Join(mapDir, "map.yaml"), []byte(mapYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTask := func(id, body string) {
		if err := os.WriteFile(filepath.Join(mapDir, "tasks", id+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Released slice card should be skipped.
	writeTask("old-card", "title: Old\nactivity: read\nrelease: r0\norder: 1\nstatus: todo\n")
	// Card with open_questions should be skipped.
	writeTask("ambiguous", "title: Ambiguous\nactivity: read\nrelease: r1\norder: 1\nstatus: todo\nopen_questions:\n  - Decide copy?\n")
	// First ready card in r1.
	writeTask("ready-one", "title: Ready One\nactivity: read\nrelease: r1\norder: 2\nstatus: todo\n")
	// Second ready card in r1.
	writeTask("ready-two", "title: Ready Two\nactivity: read\nrelease: r1\norder: 3\nstatus: todo\n")

	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New(), Workspaces: ws}

	st1, err := a.TriggerSchedule(ctx, "sandbox", "storymap-runner", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.Planning(st1.LastTaskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st1.LastStatus != "started" || !strings.Contains(st1.LastReason, "core/ready-one") {
		t.Fatalf("expected core/ready-one queued, got %+v", st1)
	}

	// Second trigger picks ready-two (ready-one already recorded in MapWorks).
	st2, err := a.TriggerSchedule(ctx, "sandbox", "storymap-runner", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for a.Planning(st2.LastTaskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st2.LastStatus != "started" || !strings.Contains(st2.LastReason, "core/ready-two") {
		t.Fatalf("expected core/ready-two queued, got %+v", st2)
	}

	// Third trigger has no ready cards left -> skipped cleanly.
	st3, err := a.TriggerSchedule(ctx, "sandbox", "storymap-runner", true)
	if err != nil {
		t.Fatal(err)
	}
	if st3.LastStatus != "skipped" {
		t.Fatalf("expected skipped when no ready cards remain, got %+v", st3)
	}
}

func TestStoryMapScheduleRespectsDependsOn(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["sandbox"].Spec.Schedules = []config.ProjectSchedule{{
		Name:         "storymap-deps",
		Every:        flow.Duration{Duration: 2 * time.Hour},
		StoryMap:     &config.ScheduleStoryMap{Map: "core", MaxPerTick: 1},
		AutoStart:    true,
		MaxOpenTasks: 5,
	}}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ws := workspace.New(t.TempDir(), nil, store.Now)
	mapDir := filepath.Join(ws.WorkDir("sandbox"), "product", "user-story-maps", "core")
	if err := os.MkdirAll(filepath.Join(mapDir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(mapDir, "map.yaml"), []byte(`title: Core
journey:
  - id: p1
    title: P1
    activities:
      - id: a1
        title: A1
releases:
  - id: r1
    title: R1
    status: in_progress
`), 0o644)
	os.WriteFile(filepath.Join(mapDir, "tasks", "prereq.yaml"), []byte("title: Prereq\nactivity: a1\nrelease: r1\norder: 1\nstatus: todo\n"), 0o644)
	os.WriteFile(filepath.Join(mapDir, "tasks", "dependent.yaml"), []byte("title: Dependent\nactivity: a1\nrelease: r1\norder: 2\nstatus: todo\ndepends_on: [prereq]\n"), 0o644)

	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New(), Workspaces: ws}

	// First trigger picks prereq.
	st1, err := a.TriggerSchedule(ctx, "sandbox", "storymap-deps", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.Planning(st1.LastTaskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(st1.LastReason, "core/prereq") {
		t.Fatalf("expected prereq first, got %+v", st1)
	}

	// While prereq has not succeeded, dependent must NOT be picked.
	st2, err := a.TriggerSchedule(ctx, "sandbox", "storymap-deps", true)
	if err != nil {
		t.Fatal(err)
	}
	if st2.LastStatus != "skipped" {
		t.Fatalf("dependent should stay blocked until prereq succeeds, got %+v", st2)
	}

	// Once prereq has a succeeded run, dependent unblocks and is picked.
	if err := st.CreateRun(ctx, &store.Run{ID: "r-prereq", FlowName: "prereq", FlowVersion: 1, TaskID: st1.LastTaskID, Project: "sandbox", Status: store.RunSucceeded, Branch: "b", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	st3, err := a.TriggerSchedule(ctx, "sandbox", "storymap-deps", true)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for a.Planning(st3.LastTaskID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st3.LastStatus != "started" || !strings.Contains(st3.LastReason, "core/dependent") {
		t.Fatalf("dependent should unblock after prereq succeeds, got %+v", st3)
	}
}
