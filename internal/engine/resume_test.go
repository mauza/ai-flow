package engine_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/store"
)

const resumeFlow = header + `  start: fix
  nodes:
    fix:
      type: agent
      model: ` + testModel + `
      prompt: fix it
      outcomes: [done]
      max_visits: 1
      next: { done: test }
    test:
      type: check
      run: make test
      next: { pass: $success, fail: fix }
`

// crash makes the current visit fail like a dead pod does.
func (h *harness) crash(runID, msg string) {
	h.t.Helper()
	v, err := h.st.LastVisit(h.ctx, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpdateVisit(h.ctx, runID, v.Seq, map[string]any{"status": store.VisitError, "error": msg, "finished_at": store.Now()}); err != nil {
		h.t.Fatal(err)
	}
	h.e.Tick(h.ctx)
}

func TestResumeFailedRunAtFailedNode(t *testing.T) {
	h := newHarness(t)
	id := h.flow(resumeFlow)
	h.finish(id, "done", nil)
	h.crash(id, "node pod failed: OOMKilled")
	if r := h.run(id); r.Status != store.RunFailed {
		t.Fatalf("want failed, got %s", r.Status)
	}

	r, err := h.e.Resume(h.ctx, id, "", "  give it more memory  ", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunRunning || r.Error != "" || r.FinishedAt != 0 || r.Resumes != 1 || r.ResumeNote != "give it more memory" {
		t.Fatalf("resumed run: %+v", r)
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	if v.Node != "test" || v.Visit != 2 || v.Status != store.VisitPending {
		t.Fatalf("resume visit: %+v", v)
	}
	h.e.Tick(h.ctx)
	if got := h.l.launched[len(h.l.launched)-1]; got.Node != "test" || got.Seq != v.Seq {
		t.Fatalf("launched %+v", got)
	}

	res, err := h.e.Resolved(h.ctx, h.run(id))
	if err != nil {
		t.Fatal(err)
	}
	md := h.e.ContextMarkdown(h.ctx, h.run(id), res, res.Nodes["test"], v.Seq)
	for _, want := range []string{"## Resumed", "stopped at test#1 (node pod failed: OOMKilled)", "> give it more memory"} {
		if !strings.Contains(md, want) {
			t.Errorf("context missing %q:\n%s", want, md)
		}
	}

	h.finish(id, "pass", nil)
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("want succeeded, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}
}

func TestResumeGivesMaxVisitsAFreshWindow(t *testing.T) {
	h := newHarness(t)
	id := h.flow(resumeFlow)
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil) // fix already ran once → $fail
	if r := h.run(id); r.Status != store.RunFailed || !strings.Contains(r.Error, "max_visits") {
		t.Fatalf("want exhausted failure, got %s: %s", r.Status, r.Error)
	}
	if _, err := h.e.Resume(h.ctx, id, "fix", "", "tester"); err != nil {
		t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	h.finish(id, "done", nil)
	h.finish(id, "fail", nil) // the resumed window allows exactly one fix visit
	r := h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(r.Error, "max_visits") {
		t.Fatalf("want exhausted again, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}
	if got := h.path(id); got != "fix:done test:fail fix:done test:fail" {
		t.Errorf("path %s", got)
	}
}

func TestResumeRejectsLiveRunsUnknownNodesAndRepeats(t *testing.T) {
	h := newHarness(t)
	id := h.flow(resumeFlow)
	if _, err := h.e.Resume(h.ctx, id, "", "", "t"); !errors.Is(err, store.ErrNotResumable) {
		t.Fatalf("running run: %v", err)
	}
	if err := h.e.Cancel(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Resume(h.ctx, id, "nope", "", "t"); err == nil || !strings.Contains(err.Error(), "not a node") {
		t.Fatalf("unknown node: %v", err)
	}
	if _, err := h.e.Resume(h.ctx, id, "", "", "t"); err != nil {
		t.Fatalf("canceled run: %v", err)
	}
	if _, err := h.e.Resume(h.ctx, id, "", "", "t"); !errors.Is(err, store.ErrNotResumable) {
		t.Fatalf("second resume: %v", err)
	}
	if r := h.run(id); r.Resumes != 1 {
		t.Fatalf("resumes %d", r.Resumes)
	}
}

func TestResumeRespectsRunSlots(t *testing.T) {
	h := newHarness(t)
	h.cfg.Env.Runs.MaxConcurrent = 1
	first := h.flow(resumeFlow)
	h.crash(first, "boom")
	h.flow(resumeFlow) // occupies the only slot
	if _, err := h.e.Resume(h.ctx, first, "", "", "t"); err == nil || !strings.Contains(err.Error(), "slots are busy") {
		t.Fatalf("want busy slots, got %v", err)
	}
	if r := h.run(first); r.Status != store.RunFailed {
		t.Fatalf("status %s", r.Status)
	}
}

func TestMaxConcurrencyIsNotConfigurationDrift(t *testing.T) {
	h := newHarness(t)
	id := h.flow(resumeFlow)
	h.cfg.Catalog.Models[testModel].MaxConcurrency = 7
	if _, err := h.e.Resolved(h.ctx, h.run(id)); err != nil {
		t.Fatalf("tuning max_concurrency drifted a pinned run: %v", err)
	}
	// Planner hints, prices and descriptions are editable in Settings while
	// runs are active.
	m := h.cfg.Catalog.Models[testModel]
	m.Notes, m.Size, m.ToolUse, m.Cost, m.InputPer1M = "edited", "large", "great", "cheap", 9
	if _, err := h.e.Resolved(h.ctx, h.run(id)); err != nil {
		t.Fatalf("editing a model's descriptive fields drifted a pinned run: %v", err)
	}
	if drifted, err := h.e.DriftedRuns(h.ctx, h.cfg); err != nil || len(drifted) != 0 {
		t.Fatalf("drifted: %v %v", drifted, err)
	}
	m.ContextTokens++
	if _, err := h.e.Resolved(h.ctx, h.run(id)); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("a real model change must still be drift: %v", err)
	}
	if drifted, _ := h.e.DriftedRuns(h.ctx, h.cfg); len(drifted) != 1 || drifted[0] != id {
		t.Fatalf("DriftedRuns must name the run: %v", drifted)
	}
}
