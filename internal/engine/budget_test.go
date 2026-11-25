package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func ago(d time.Duration) int64 { return time.Now().Add(-d).UnixMilli() }

func (h *harness) ticks(n int) {
	for range n {
		h.e.Tick(h.ctx)
	}
}

const tokenFlow = `  budget: { tokens: 1000 }
  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: do it
      outcomes: [done, again]
      max_visits: 5
      next: { done: test, again: work }
    test:
      type: check
      run: make test
      next: { pass: $success, fail: work }
`

func TestTokenBudgetExcludesCachedTokens(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + tokenFlow)
	must(t, h.st.AddUsage(h.ctx, id, 1, testModel, 9000, 100, 8500, 0))
	h.finish(id, "again", nil)
	if v := h.current(id); h.run(id).Status != store.RunRunning || v.Visit != 2 {
		t.Fatalf("600 billable tokens of 1000 ended the run: %s %q", h.run(id).Status, h.run(id).Error)
	}
	must(t, h.st.AddUsage(h.ctx, id, 2, testModel, 400, 50, 0, 0))
	h.finish(id, "again", nil)
	if r := h.run(id); r.Status != store.RunFailed || !strings.Contains(r.Error, "token budget of 1000") {
		t.Fatalf("over budget: %s %q", r.Status, r.Error)
	}
	// A resume opens a fresh window.
	if _, err := h.e.Resume(h.ctx, id, "", "", "me"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2)
	if r := h.run(id); r.Status != store.RunRunning {
		t.Fatalf("resume: %s %q", r.Status, r.Error)
	}
}

func TestSpentTokenBudgetStillRunsChecks(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + tokenFlow)
	must(t, h.st.AddUsage(h.ctx, id, 1, testModel, 5000, 0, 0, 0))
	h.ticks(1)
	if h.run(id).Status != store.RunRunning {
		t.Fatal("the budget killed a step under way")
	}
	h.finish(id, "done", nil)
	h.finish(id, "pass", nil)
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("%s %q", r.Status, r.Error)
	}
}

const wallFlow = `  budget: { wall: 10m }
  start: step
  nodes:
    step:
      type: check
      run: make test
      next: { pass: ask, fail: $fail }
    ask:
      type: gate
      prompt: ship?
      timeout: 48h
      outcomes: [approve, stop]
      next: { approve: last, stop: $fail, timeout: $fail }
    last:
      type: check
      run: make test
      next: { pass: $success, fail: $fail }
`

func TestResumeOpensAFreshWallBudget(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + wallFlow)
	must(t, h.st.UpdateRun(h.ctx, id, map[string]any{"started_at": ago(time.Hour)}))
	h.ticks(1)
	if r := h.run(id); r.Status != store.RunFailed || !strings.Contains(r.Error, "wall-clock") {
		t.Fatalf("%s %q", r.Status, r.Error)
	}
	if _, err := h.e.Resume(h.ctx, id, "", "", "me"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2)
	if r := h.run(id); r.Status != store.RunRunning {
		t.Fatalf("resume: %s %q", r.Status, r.Error)
	}
	must(t, h.st.UpdateVisit(h.ctx, id, h.current(id).Seq, map[string]any{"created_at": ago(time.Hour)}))
	h.ticks(1)
	if r := h.run(id); r.Status != store.RunFailed {
		t.Fatalf("the window after a resume is unbounded: %s", r.Status)
	}
}

func TestWallBudgetExcludesWaitingForPeople(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + wallFlow)
	h.finish(id, "pass", nil)
	gate := h.current(id)
	must(t, h.st.UpdateRun(h.ctx, id, map[string]any{"started_at": ago(5 * time.Hour)}))
	must(t, h.st.UpdateVisit(h.ctx, id, gate.Seq, map[string]any{"started_at": ago(5*time.Hour - time.Minute)}))
	h.ticks(2)
	if r := h.run(id); r.Status != store.RunWaiting {
		t.Fatalf("failed while waiting for a person: %s %q", r.Status, r.Error)
	}
	h.finish(id, "approve", nil)
	h.ticks(2)
	if r := h.run(id); r.Status != store.RunRunning || h.current(id).Node != "last" {
		t.Fatalf("after the decision: %s %q", r.Status, r.Error)
	}
}

type flakyKill struct {
	*fakeLauncher
	fails int
}

func (f *flakyKill) Kill(ctx context.Context, name string) error {
	if f.fails > 0 {
		f.fails--
		return errors.New("apiserver timeout")
	}
	return f.fakeLauncher.Kill(ctx, name)
}

func TestFailFastSurvivesAFailedKill(t *testing.T) {
	h := newHarness(t)
	fk := &flakyKill{fakeLauncher: h.l, fails: 1}
	h.e = engine.New(h.cfg, h.st, fk, hub.New(), nil)
	id := h.flow(header + `  start: checks
  nodes:
    checks:
      type: parallel
      fail_fast: true
      branches: [unit, slow]
      join: gather
    unit: { type: check, run: make test, next: { pass: gather, fail: gather } }
    slow: { type: check, run: make slow, next: { pass: gather, fail: gather } }
    gather:
      type: join
      cases: [{ when: 'nodes.unit.outcome == "fail"', outcome: red }]
      default: green
      next: { green: $success, red: fixup }
    fixup: { type: check, run: make fix, next: { pass: $success, fail: $fail } }
`)
	h.finishNode(id, "unit", "fail")
	h.ticks(3)
	if r := h.run(id); r.Status != store.RunRunning || h.current(id).Node != "fixup" {
		t.Fatalf("%s %q at %s", r.Status, r.Error, h.path(id))
	}
	for _, s := range h.l.launched {
		if s.Node == "slow" && h.l.state[engine.JobName(s)] != engine.JobFailed {
			t.Fatal("the sibling's job was never stopped")
		}
	}
}
