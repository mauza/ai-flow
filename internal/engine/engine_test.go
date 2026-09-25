package engine_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

// fakeLauncher records launches; tests decide what each job "does".
type fakeLauncher struct {
	mu       sync.Mutex
	launched []engine.LaunchSpec
	state    map[string]engine.JobState
}

func (f *fakeLauncher) Launch(_ context.Context, s engine.LaunchSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, s)
	name := s.Node + "-" + string(rune('0'+s.Seq))
	f.state[name] = engine.JobRunning
	return name, nil
}

func (f *fakeLauncher) Status(_ context.Context, name string) (engine.JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.state[name]
	if !ok {
		return engine.JobStatus{State: engine.JobMissing}, nil
	}
	return engine.JobStatus{State: st}, nil
}

func (f *fakeLauncher) Kill(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[name] = engine.JobFailed
	return nil
}

type harness struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	e   *engine.Engine
	l   *fakeLauncher
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := &fakeLauncher{state: map[string]engine.JobState{}}
	e := engine.New(cfg, st, l, hub.New(), nil)
	e.OrphanGrace = 0
	return &harness{t: t, ctx: context.Background(), st: st, e: e, l: l}
}

func (h *harness) flow(yaml string) string {
	h.t.Helper()
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", YAML: yaml, Project: "sandbox"}); err != nil {
		h.t.Fatal(err)
	}
	r, err := h.e.CreateRun(h.ctx, "f", 0, "")
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	return r.ID
}

// finish reports a result for the run's current visit, like the broker does.
func (h *harness) finish(runID, outcome string, outputs map[string]any) {
	h.t.Helper()
	v, err := h.st.LastVisit(h.ctx, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	if outputs == nil {
		outputs = map[string]any{}
	}
	raw, _ := json.Marshal(outputs)
	if err := h.st.UpdateVisit(h.ctx, runID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": outcome, "outputs": string(raw), "finished_at": store.Now()}); err != nil {
		h.t.Fatal(err)
	}
	h.e.Tick(h.ctx)
}

func (h *harness) run(id string) *store.Run {
	h.t.Helper()
	r, err := h.st.GetRun(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) path(id string) string {
	vs, _ := h.st.Visits(h.ctx, id)
	var p []string
	for _, v := range vs {
		p = append(p, v.Node+":"+v.Outcome)
	}
	return strings.Join(p, " ")
}

const header = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: f, project: sandbox }
spec:
`

func TestHappyPathAndLoop(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      max_visits: 3
      next: { done: test }
    test:
      type: check
      run: make test
      next: { pass: $success, fail: fix }
`)
	if r := h.run(id); r.Status != store.RunRunning || r.CurrentNode != "fix" {
		t.Fatalf("after start: %s at %s", r.Status, r.CurrentNode)
	}
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil) // test fails → back to fix
	h.finish(id, "done", nil)
	h.finish(id, "pass", nil)
	r := h.run(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("status %s (%s), path %s", r.Status, r.Error, h.path(id))
	}
	if got := h.path(id); got != "fix:done test:fail fix:done test:pass" {
		t.Errorf("path %s", got)
	}
	if len(h.l.launched) != 4 || h.l.launched[2].Visit != 2 {
		t.Errorf("launches %+v", h.l.launched)
	}
}

func TestMaxVisitsExhausted(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      max_visits: 2
      on_exhausted: escalate
      next: { done: test }
    test:
      type: check
      run: make test
      next: { pass: $success, fail: fix }
    escalate:
      type: gate
      prompt: stuck
      outcomes: [give_up]
      next: { give_up: $fail }
`)
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil)
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil) // fix already ran twice → escalate gate
	r := h.run(id)
	if r.Status != store.RunWaiting || r.CurrentNode != "escalate" {
		t.Fatalf("want waiting at escalate, got %s at %s (%s)", r.Status, r.CurrentNode, h.path(id))
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	if err := h.e.Decide(h.ctx, id, v.Seq, "nope", "t"); err == nil {
		t.Error("deciding an unknown outcome should fail")
	}
	if err := h.e.Decide(h.ctx, id, v.Seq, "give_up", "tester"); err != nil {
		t.Fatal(err)
	}
	r = h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(r.Error, "escalate → give_up") {
		t.Errorf("got %s: %s", r.Status, r.Error)
	}
}

func TestExhaustedToFailExplains(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      max_visits: 1
      next: { done: test }
    test:
      type: check
      run: make test
      next: { pass: $success, fail: fix }
`)
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil)
	r := h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(r.Error, "max_visits") {
		t.Errorf("got %s: %q", r.Status, r.Error)
	}
}

func TestSwitchUsesRunDiffAndOutputs(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: size
  nodes:
    size:
      type: llm
      model: gemma-local
      prompt: estimate
      outputs: { files: number }
      outcomes: [done]
      next: { done: route }
    route:
      type: switch
      cases:
        - when: nodes.size.outputs.files > 3
          outcome: big
      default: small
      next: { big: $fail, small: $success }
`)
	h.finish(id, "done", map[string]any{"files": 2})
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("want succeeded via small, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}
}

func TestCrashedPodFailsRun(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      next: { done: $success }
`)
	v, _ := h.st.LastVisit(h.ctx, id)
	h.l.state[v.JobName] = engine.JobFailed
	h.e.Tick(h.ctx) // first sighting starts the grace period
	time.Sleep(5 * time.Millisecond)
	h.e.Tick(h.ctx)
	r := h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(r.Error, "node pod failed") {
		t.Errorf("got %s: %s", r.Status, r.Error)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      next: { done: $success }
`)
	if err := h.e.Cancel(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if r := h.run(id); r.Status != store.RunCanceled {
		t.Errorf("status %s", r.Status)
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	if v.Status != store.VisitCanceled || h.l.state[v.JobName] != engine.JobFailed {
		t.Errorf("visit %s, job %s", v.Status, h.l.state[v.JobName])
	}
	// a late result must not resurrect the run
	h.finish(id, "done", nil)
	if r := h.run(id); r.Status != store.RunCanceled {
		t.Errorf("late result changed status to %s", r.Status)
	}
}

func TestInvalidFlowIsRejected(t *testing.T) {
	h := newHarness(t)
	h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "bad", YAML: header + "  start: nope\n  nodes: {}\n"})
	if _, err := h.e.CreateRun(h.ctx, "bad", 0, ""); err == nil {
		t.Fatal("invalid flow started")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newHarness(t)
	src := header + `  start: fix
  nodes:
    fix:
      type: agent
      model: gemma-local
      prompt: fix it
      outcomes: [done]
      next: { done: $success }
`
	h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", YAML: src})
	var ids []string
	for i := 0; i < 5; i++ {
		r, err := h.e.CreateRun(h.ctx, "f", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	h.e.Tick(h.ctx)
	running := 0
	for _, id := range ids {
		if h.run(id).Status == store.RunRunning {
			running++
		}
	}
	if running != 3 { // maxConcurrent in deploy/config
		t.Errorf("running %d, want 3", running)
	}
	if h.run(ids[0]).Status != store.RunRunning {
		t.Error("oldest run should start first")
	}
}
