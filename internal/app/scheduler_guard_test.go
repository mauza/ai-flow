package app_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/workspace"
)

type schedEnv struct {
	t   *testing.T
	ctx context.Context
	a   *app.App
	st  *store.Store
	cfg *config.Config
}

// newSched has a store and an engine that only creates (never steps) runs. The
// planner is missing, so planning fails at once; ready sets what it would save.
func newSched(t *testing.T, scheds ...config.ProjectSchedule) *schedEnv {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["sandbox"].Spec.Start = "manual"
	cfg.Projects["sandbox"].Spec.Schedules = scheds
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	a := &app.App{Cfg: cfg, Store: st, Hub: h, Engine: engine.New(cfg, st, nil, h, nil)}
	return &schedEnv{t, context.Background(), a, st, cfg}
}

func (e *schedEnv) trigger(name string, force bool) *app.ScheduleState {
	e.t.Helper()
	s, err := e.a.TriggerSchedule(e.ctx, "sandbox", name, force)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// ready gives the task a valid flow, as a finished planner would.
func (e *schedEnv) ready(id string) {
	e.t.Helper()
	for e.a.Planning(id) {
		time.Sleep(5 * time.Millisecond)
	}
	name := "flow-" + id
	src := fmt.Sprintf(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: %s, project: sandbox }
spec:
  start: step
  nodes:
    step: { type: check, run: make test, next: { pass: $success, fail: $fail } }
`, name)
	if _, err := e.st.SaveFlow(e.ctx, &store.FlowVersion{Name: name, YAML: src, Project: "sandbox", TaskID: id}); err != nil {
		e.t.Fatal(err)
	}
	e.set(id, store.TaskFlowReady)
	if err := e.st.UpdateTask(e.ctx, id, map[string]any{"flow_name": name}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *schedEnv) set(id, status string) {
	e.t.Helper()
	if err := e.st.UpdateTask(e.ctx, id, map[string]any{"status": status, "error": ""}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *schedEnv) runs(taskID string) int {
	e.t.Helper()
	rs, err := e.st.ListRuns(e.ctx, store.RunFilter{TaskID: taskID})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(rs)
}

func (e *schedEnv) tick() { e.a.TickSchedules(e.ctx) }

func taskSched(name string) config.ProjectSchedule {
	return config.ProjectSchedule{Name: name, Every: flow.Duration{Duration: 24 * time.Hour}, AutoStart: true,
		Task: &config.ScheduleTask{Title: "Audit " + name}}
}

func TestQueuedTaskHoldsTheOnlySlot(t *testing.T) {
	e := newSched(t, taskSched("one"), taskSched("two"))
	one := e.trigger("one", false)
	e.ready(one.LastTaskID)
	two := e.trigger("two", false)
	if two.LastStatus != "skipped" {
		t.Fatalf("second schedule took a slot that is spoken for: %+v", two)
	}
	if two.LastRunAt != 0 || two.RetryAt == 0 || two.RetryAt > store.Now()+11*60_000 {
		t.Fatalf("a skip must not use up the interval: %+v", two)
	}
	e.tick()
	if e.runs(one.LastTaskID) != 1 {
		t.Fatal("queued task was not started")
	}
	e.tick()
	if e.runs(one.LastTaskID) != 1 {
		t.Fatal("queued task was started twice")
	}
	if s := e.trigger("two", false); s.LastStatus != "skipped" {
		t.Fatalf("one run is active: %+v", s)
	}
}

func TestTasksWaitingForAHumanDoNotBlockSchedules(t *testing.T) {
	sc := taskSched("one")
	sc.MaxOpenTasks = 1
	e := newSched(t, sc)
	for _, k := range []string{"a", "b", "c"} {
		id, _, err := e.a.CreateTaskFromFlow(e.ctx, engine.ChildTask{Project: "sandbox", Key: k, Title: k})
		if err != nil {
			t.Fatal(err)
		}
		e.ready(id)
	}
	if s := e.trigger("one", false); s.LastStatus != "started" {
		t.Fatalf("blocked by tasks nobody will start: %+v", s)
	}
}

func TestChildTasksStartUnderTheirParentsSchedule(t *testing.T) {
	e := newSched(t, taskSched("audit"))
	parent := e.trigger("audit", false).LastTaskID
	e.ready(parent)
	e.tick()
	// The audit spawns two tasks while its run is still active.
	var kids []string
	for _, k := range []string{"a", "b"} {
		id, created, err := e.a.CreateTaskFromFlow(e.ctx, engine.ChildTask{Project: "sandbox", Key: k, Title: k, Plan: true, ParentTaskID: parent})
		if err != nil || !created {
			t.Fatal(id, created, err)
		}
		e.ready(id)
		kids = append(kids, id)
	}
	e.tick()
	if e.runs(kids[0])+e.runs(kids[1]) != 0 {
		t.Fatal("children started while the only slot was taken")
	}
	finish := func(task string) {
		rs, _ := e.st.ListRuns(e.ctx, store.RunFilter{TaskID: task})
		if err := e.st.SetRunState(e.ctx, rs[0], store.RunSucceeded, "", store.Now()); err != nil {
			t.Fatal(err)
		}
	}
	finish(parent)
	if err := e.a.PauseSchedule(e.ctx, "sandbox", "audit", true); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if e.runs(kids[0]) != 0 {
		t.Fatal("a paused schedule started a run")
	}
	if _, err := e.a.TriggerSchedule(e.ctx, "sandbox", "audit", false); err == nil {
		t.Fatal("a paused schedule was triggered")
	}
	if v := e.a.ListSchedules(e.ctx, "sandbox")[0]; !v.Paused || v.NextDueAt != 0 {
		t.Fatalf("view: %+v", v)
	}
	if err := e.a.PauseSchedule(e.ctx, "sandbox", "audit", false); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if e.runs(kids[0]) != 1 || e.runs(kids[1]) != 0 {
		t.Fatalf("want the oldest child alone: %d %d", e.runs(kids[0]), e.runs(kids[1]))
	}
	finish(kids[0])
	e.tick()
	if e.runs(kids[1]) != 1 {
		t.Fatal("second child never started")
	}
}

func TestInterruptedPlanningIsRetriedOnce(t *testing.T) {
	e := newSched(t, taskSched("one"))
	id := e.trigger("one", false).LastTaskID
	for i, want := range []string{store.TaskPlanFailed, store.TaskPlanning} {
		for e.a.Planning(id) {
			time.Sleep(5 * time.Millisecond)
		}
		e.set(id, store.TaskPlanning) // what a restart mid-planning leaves behind
		e.tick()
		for e.a.Planning(id) {
			time.Sleep(5 * time.Millisecond)
		}
		if task, _ := e.st.GetTask(e.ctx, id); task.Status != want {
			t.Fatalf("interruption %d: status %s, want %s", i+1, task.Status, want)
		}
	}
}

func TestOpenPullRequestsBlockSchedules(t *testing.T) {
	heads, fail := []string{"ai-flow/a", "feature/mine"}, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(500)
			return
		}
		var out []string
		for _, h := range heads {
			out = append(out, fmt.Sprintf(`{"head":{"ref":%q}}`, h))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(out, ","))
	}))
	defer srv.Close()
	e := newSched(t, taskSched("one"))
	e.cfg.Catalog.Grants["repo/sandbox"] = &config.Grant{Kind: config.GrantGit, URL: "https://github.com/o/r.git"}
	e.cfg.Projects["sandbox"].Spec.Repo = "repo/sandbox"
	e.a.GitHub = github.New(srv.URL, "t")

	heads = append(heads, "ai-flow/b")
	if s := e.trigger("one", false); s.LastStatus != "skipped" || !strings.Contains(s.LastReason, "2 ai-flow pull request") {
		t.Fatalf("two open PRs: %+v", s)
	}
	fail = true
	if _, err := e.a.TriggerSchedule(e.ctx, "sandbox", "one", false); err == nil {
		t.Fatal("unknown PR count must not count as zero")
	}
	fail, heads = false, heads[:1]
	s := e.trigger("one", false)
	if s.LastStatus != "started" {
		t.Fatalf("one open PR: %+v", s)
	}
	e.ready(s.LastTaskID)
	heads = append(heads, "ai-flow/c")
	e.tick()
	if e.runs(s.LastTaskID) != 0 {
		t.Fatal("queued run started past max_open_prs")
	}
}

func TestChildTaskDedup(t *testing.T) {
	e := newSched(t)
	other := *e.cfg.Projects["sandbox"]
	other.Metadata.Name = "other"
	e.cfg.Projects["other"] = &other
	create := func(project string) (string, bool) {
		t.Helper()
		id, created, err := e.a.CreateTaskFromFlow(e.ctx, engine.ChildTask{Project: project, Source: "flow", Key: "k", Title: "T"})
		if err != nil {
			t.Fatal(err)
		}
		return id, created
	}
	first, ok := create("sandbox")
	if !ok {
		t.Fatal("first")
	}
	if id, ok := create("sandbox"); ok || id != first {
		t.Fatal("duplicate created")
	}
	if _, ok := create("other"); !ok {
		t.Fatal("another project's task counted as a duplicate")
	}
	e.set(first, store.TaskFailed)
	second, ok := create("sandbox")
	if !ok {
		t.Fatal("a failed task should be proposable again")
	}
	e.set(second, store.TaskSucceeded)
	if _, ok := create("sandbox"); ok {
		t.Fatal("delivered work proposed again")
	}
	if err := e.a.Engine.UpsertLedger(e.ctx, "sandbox", engine.LedgerEntry{Key: "k", Status: engine.LedgerReverted}); err != nil {
		t.Fatal(err)
	}
	third, ok := create("sandbox")
	if !ok {
		t.Fatal("a reverted change should be proposable again")
	}
	e.set(third, store.TaskFailed)
	if _, ok := create("sandbox"); ok {
		t.Fatal("attempts are not bounded")
	}
}

func TestLedgerFollowsThePullRequest(t *testing.T) {
	states := map[string]string{"1": `{"state":"closed","merged":true}`, "2": `{"state":"closed"}`, "3": `{"state":"open"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, states[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]])
	}))
	defer srv.Close()
	e := newSched(t)
	e.a.GitHub = github.New(srv.URL, "t")
	for n := range states {
		if err := e.a.Engine.UpsertLedger(e.ctx, "sandbox", engine.LedgerEntry{Key: "k" + n, Title: n, Status: engine.LedgerPROpen, PRURL: "https://github.com/o/r/pull/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	e.a.ReconcileLedgers(e.ctx)
	want := map[string]string{"k1": engine.LedgerShipped, "k2": engine.LedgerRejected, "k3": engine.LedgerPROpen}
	for _, it := range e.a.Engine.Ledger(e.ctx, "sandbox") {
		if it.Status != want[it.Key] {
			t.Errorf("%s: %s, want %s", it.Key, it.Status, want[it.Key])
		}
	}
}

func TestFailedCardIsRetriedOnceAfterAnInterval(t *testing.T) {
	sc := config.ProjectSchedule{Name: "map", Every: flow.Duration{Duration: 4 * time.Hour}, AutoStart: true, MaxActiveRuns: 5,
		StoryMap: &config.ScheduleStoryMap{Map: "core"}}
	e := newSched(t, sc)
	e.a.Workspaces = workspace.New(t.TempDir(), nil, store.Now)
	dir := filepath.Join(e.a.Workspaces.WorkDir("sandbox"), "product", "user-story-maps", "core")
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "map.yaml"), []byte("title: Core\njourney:\n  - id: d\n    title: D\n    activities:\n      - id: read\n        title: Read\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "tasks", "card.yaml"), []byte("title: Card\nactivity: read\norder: 1\nstatus: todo\n"), 0o644)
	fail := func(id string) {
		t.Helper()
		for e.a.Planning(id) {
			time.Sleep(5 * time.Millisecond)
		}
		if err := e.st.UpdateTask(e.ctx, id, map[string]any{"status": store.TaskFailed, "error": "QA was blocked"}); err != nil {
			t.Fatal(err)
		}
	}
	first := e.trigger("map", false)
	fail(first.LastTaskID)
	if s := e.trigger("map", false); s.LastStatus != "skipped" {
		t.Fatalf("a fresh failure leaves time to resume it: %+v", s)
	}
	// Let the interval pass.
	e.cfg.Projects["sandbox"].Spec.Schedules[0].Every.Duration = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	second := e.trigger("map", false)
	if second.LastStatus != "started" {
		t.Fatalf("no retry: %+v", second)
	}
	if task, _ := e.st.GetTask(e.ctx, second.LastTaskID); !strings.Contains(task.Body, "QA was blocked") {
		t.Fatalf("retry brief lacks the failure: %s", task.Body)
	}
	fail(second.LastTaskID)
	time.Sleep(5 * time.Millisecond)
	if s := e.trigger("map", false); s.LastStatus != "skipped" {
		t.Fatalf("two failures are enough: %+v", s)
	}
}
