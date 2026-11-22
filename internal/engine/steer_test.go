package engine_test

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/store"
)

func TestSteerPauseAndNoteMidFlight(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: step1
  nodes:
    step1:
      type: agent
      model: ` + testModel + `
      prompt: First step
      outcomes: [done]
      next: { done: step2 }
    step2:
      type: agent
      model: ` + testModel + `
      prompt: "Second step (steer: ${{ run.steer_note ?? \"none\" }})"
      outcomes: [done]
      next: { done: $success }
`)
	// While step1 is running, ask to pause after it finishes and leave a note.
	pause := true
	note := "Focus on mobile layout"
	r, err := h.e.Steer(h.ctx, id, &pause, &note, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !r.PauseRequested || r.SteerNote != note {
		t.Fatalf("steer state: %+v", r)
	}

	// Finishing step1 enters a synthetic $pause gate instead of launching step2.
	h.finish(id, "done", nil)
	r = h.run(id)
	if r.Status != store.RunWaiting || r.CurrentNode != flow.NodePause || r.PauseRequested {
		t.Fatalf("expected pause waiting, got status=%s node=%s pause=%v", r.Status, r.CurrentNode, r.PauseRequested)
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	if v.Node != flow.NodePause || v.Status != store.VisitWaiting {
		t.Fatalf("pause visit: %+v", v)
	}

	// Continue from the pause gate with an updated direction note.
	if err := h.e.Decide(h.ctx, id, v.Seq, flow.OutcomeContinue, "alice", "Also keep the header sticky"); err != nil {
		t.Fatal(err)
	}
	step2, _ := h.st.LastVisit(h.ctx, id)
	if step2.Node != "step2" || step2.LaunchState != "launched" {
		t.Fatalf("expected step2 launched, got %+v", step2)
	}
	r = h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["step2"], step2.Seq)
	if !strings.Contains(md, "## Human steering") || !strings.Contains(md, "Also keep the header sticky") {
		t.Errorf("step2 context lacks steering note:\n%s", md)
	}
	c := h.e.TemplateContext(h.ctx, r, res, res.Nodes["step2"])
	prompt, _ := engine.RenderText(res.Nodes["step2"].Prompt, c)
	if prompt != "Second step (steer: Also keep the header sticky)" {
		t.Errorf("prompt %q", prompt)
	}

	h.finish(id, "done", nil)
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("status %s: %s", r.Status, r.Error)
	}
}

func TestGateExtendVisitsAllowsExtraHumanRevisionRound(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: Build it
      outcomes: [done]
      max_visits: 1
      on_exhausted: $fail
      next: { done: approve }
    approve:
      type: gate
      prompt: Looks good?
      outcomes: [approve, revise]
      max_visits: 1
      on_exhausted: $fail
      next: { approve: $success, revise: work }
`)
	// First pass finishes work#1 and reaches approve#1.
	h.finish(id, "done", nil)
	gate, _ := h.st.LastVisit(h.ctx, id)

	// Without extend_visits, revising would immediately exhaust work (max_visits: 1).
	// With extendVisits=true, the human grants +1 loop round to work and approve.
	if err := h.e.DecideWithOptions(h.ctx, id, gate.Seq, "revise", "bob", "Try a cleaner UX", true); err != nil {
		t.Fatal(err)
	}
	r := h.run(id)
	if r.Status != store.RunRunning || r.CurrentNode != "work" || r.ExtraVisits != 1 {
		t.Fatalf("expected work#2 running with extra_visits=1, got status=%s node=%s extra=%d", r.Status, r.CurrentNode, r.ExtraVisits)
	}
	h.finish(id, "done", nil)
	gate2, _ := h.st.LastVisit(h.ctx, id)
	if gate2.Node != "approve" || gate2.Status != store.VisitWaiting {
		t.Fatalf("expected approve#2 waiting, got %+v", gate2)
	}
	if err := h.e.Decide(h.ctx, id, gate2.Seq, "approve", "bob", ""); err != nil {
		t.Fatal(err)
	}
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("status %s: %s", r.Status, r.Error)
	}
}
