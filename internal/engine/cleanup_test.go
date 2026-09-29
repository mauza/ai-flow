package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

type cleanupLauncher struct {
	*fakeLauncher
	failures int
	killed   []string
}

func (l *cleanupLauncher) Kill(ctx context.Context, name string) error {
	l.killed = append(l.killed, name)
	if l.failures > 0 {
		l.failures--
		return errors.New("temporary job cleanup failure")
	}
	return l.fakeLauncher.Kill(ctx, name)
}

func TestRejectedUncertainLaunchCleansUpBeforeFreeingSlot(t *testing.T) {
	for _, rejection := range []string{"revoked", "drift"} {
		for _, failures := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/kill-failures-%d", rejection, failures), func(t *testing.T) {
				h := newHarness(t)
				h.cfg.Env.Runs.MaxConcurrent = 1
				r := h.queue(askFlow)
				h.e = engine.New(h.cfg, h.st, &uncertainLauncher{fakeLauncher: h.l, fail: true}, hub.New(), nil)
				h.e.Tick(h.ctx) // Job was created, but the response was lost.
				v, err := h.st.LastVisit(h.ctx, r.ID)
				must(t, err)
				if v.LaunchState != "pending" || h.l.state[v.JobName] != engine.JobRunning {
					t.Fatalf("missing uncertain live job: %+v", v)
				}
				h.restart()
				originalModels := h.cfg.Projects["sandbox"].Spec.Allow.Models
				originalModel := h.cfg.Catalog.Models[testModel].Model
				if rejection == "revoked" {
					h.cfg.Projects["sandbox"].Spec.Allow.Models = []string{"other-model"}
				} else {
					h.cfg.Catalog.Models[testModel].Model = "changed-model"
				}
				good := h.queue(checkFlow)
				l := &cleanupLauncher{fakeLauncher: h.l, failures: failures}
				h.e = engine.New(h.cfg, h.st, l, hub.New(), nil)
				h.e.Tick(h.ctx)
				if len(l.killed) != 1 || l.killed[0] != v.JobName {
					t.Fatalf("persisted Job not cleaned up: %v", l.killed)
				}
				if failures > 0 {
					if h.run(r.ID).Status != store.RunRunning || h.run(good.ID).Status != store.RunQueued || len(h.l.launched) != 1 || h.l.state[v.JobName] != engine.JobRunning {
						t.Fatal("failed cleanup terminalized the run or freed its slot")
					}
					accepted, err := h.st.UpdateVisitIf(h.ctx, r.ID, v.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{"status": store.VisitSucceeded, "outcome": "done"})
					must(t, err)
					if accepted {
						t.Fatal("late result erased pending cleanup")
					}
					// Cleanup intent must survive restart and a later configuration
					// repair; the rejected visit must not resume execution instead.
					h.cfg.Projects["sandbox"].Spec.Allow.Models = originalModels
					h.cfg.Catalog.Models[testModel].Model = originalModel
					h.restart()
					h.e = engine.New(h.cfg, h.st, l, hub.New(), nil)
					h.e.Tick(h.ctx)
					if len(l.killed) != 2 || l.killed[1] != v.JobName {
						t.Fatalf("cleanup was not retried: %v", l.killed)
					}
				}
				if h.run(r.ID).Status != store.RunFailed || h.l.state[v.JobName] != engine.JobFailed {
					t.Fatal("rejected run finished without stopping its Job")
				}
				if h.run(good.ID).Status != store.RunRunning || len(h.l.launched) != 2 {
					t.Fatal("successful cleanup did not release capacity for the queued run")
				}
			})
		}
	}
}

func TestFailedPodCleanupRetriesBeforeTerminalization(t *testing.T) {
	for _, reason := range []string{"reported-error", "timeout"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			r := h.queue(checkFlow)
			h.e.Tick(h.ctx)
			v, err := h.st.LastVisit(h.ctx, r.ID)
			must(t, err)
			if reason == "reported-error" {
				must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"status": store.VisitError, "error": "runner reported failure"}))
			} else {
				must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"deadline": store.Now() - 120_000}))
			}
			l := &cleanupLauncher{fakeLauncher: h.l, failures: 1}
			h.e = engine.New(h.cfg, h.st, l, hub.New(), nil)
			h.e.Tick(h.ctx)
			if h.run(r.ID).Status != store.RunRunning || len(l.killed) != 1 {
				t.Fatal("failed pod cleanup did not retain the running run")
			}
			h.restart()
			h.e = engine.New(h.cfg, h.st, l, hub.New(), nil)
			h.e.Tick(h.ctx)
			if h.run(r.ID).Status != store.RunFailed || len(l.killed) != 2 || l.killed[1] != v.JobName || h.l.state[v.JobName] != engine.JobFailed {
				t.Fatal("failed pod cleanup did not complete after retry")
			}
		})
	}
}
