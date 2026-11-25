package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

func TestRunWallBudgetEnforced(t *testing.T) {
	t.Run("wall", func(t *testing.T) {
		h := newHarness(t)
		id := h.flow(header + `  budget: { wall: 1m }
  start: step
  nodes:
    step:
      type: check
      run: make test
      next: { pass: $success, fail: $fail }
`)
		// Backdate started_at past the 1m wall-clock budget.
		past := time.Now().Add(-2 * time.Minute).UnixMilli()
		must(t, h.st.UpdateRun(h.ctx, id, map[string]any{"started_at": past}))
		h.e.Tick(h.ctx)
		r := h.run(id)
		if r.Status != store.RunFailed || !strings.Contains(r.Error, "wall-clock budget") {
			t.Fatalf("expected wall budget failure, got %s: %q", r.Status, r.Error)
		}
	})
}

func TestFailureReportPreservesRootCause(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: check
      run: make test
      max_visits: 1
      on_exhausted: publish_failure
      next: { pass: $success, fail: work }
    publish_failure:
      type: action
      action: comment_task
      with: { body: "failed" }
      next: { done: $fail }
`)
	h.finish(id, "fail", nil)
	r := h.run(id)
	if r.Status != store.RunFailed {
		t.Fatalf("status %s", r.Status)
	}
	if !strings.Contains(r.Error, "work already ran 1 times") || !strings.Contains(r.Error, "publish_failure") {
		t.Fatalf("root cause masked in error: %q", r.Error)
	}
}

func TestParallelFailFastCancelsSiblings(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: checks
  nodes:
    checks:
      type: parallel
      fail_fast: true
      branches: [unit, slow_qa]
      join: gather
    unit:
      type: check
      run: make test
      next: { pass: gather, fail: gather }
    slow_qa:
      type: agent
      model: ` + testModel + `
      prompt: qa
      outcomes: [passed, defects]
      next: { passed: gather, defects: gather }
    gather:
      type: join
      cases:
        - when: 'nodes.unit.outcome == "fail"'
          outcome: red
        - when: 'nodes.unit.outcome == "pass" && nodes.slow_qa.outcome == "passed"'
          outcome: green
      default: red
      next: { green: $success, red: $fail }
`)
	if got := h.activeNodes(id); got != "checks,slow_qa,unit" {
		t.Fatalf("expected both branches active: %s", got)
	}
	h.finishNode(id, "unit", "fail")
	r := h.run(id)
	if r.Status != store.RunFailed {
		t.Fatalf("expected fail-fast run to reach $fail via gather, got %s (%s)", r.Status, h.path(id))
	}
	// slow_qa job must have been killed without waiting for it.
	for _, s := range h.l.launched {
		if s.Node == "slow_qa" {
			if st := h.l.state[engine.JobName(s)]; st != engine.JobFailed {
				t.Errorf("slow_qa job not killed on fail_fast: state=%s", st)
			}
		}
	}
}
