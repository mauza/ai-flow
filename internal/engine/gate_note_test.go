package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

func TestGateNoteReachesTheNextStep(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: "Do the work. Reviewer said: ${{ nodes.approve.outputs.note ?? \"nothing yet\" }}"
      outcomes: [done]
      max_visits: 2
      next: { done: approve }
    approve:
      type: gate
      prompt: Ship it?
      outcomes: [approve, revise]
      next: { approve: $success, revise: work }
`)
	h.finish(id, "done", nil)
	gate, _ := h.st.LastVisit(h.ctx, id)
	if err := h.e.Decide(h.ctx, id, gate.Seq, "revise", "casey", "  Use the existing slug helper instead.  "); err != nil {
		t.Fatal(err)
	}
	gate, _ = h.st.GetVisit(h.ctx, id, gate.Seq)
	var outputs map[string]string
	if err := json.Unmarshal(gate.Outputs, &outputs); err != nil || outputs["note"] != "Use the existing slug helper instead." {
		t.Fatalf("gate outputs %s (%v)", gate.Outputs, err)
	}
	if !strings.HasSuffix(gate.Summary, `by casey: Use the existing slug helper instead.`) {
		t.Errorf("summary %q", gate.Summary)
	}

	work, _ := h.st.LastVisit(h.ctx, id)
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["work"], work.Seq)
	if !strings.Contains(md, "## Previous step: approve → revise") || !strings.Contains(md, "Use the existing slug helper instead.") {
		t.Errorf("context lacks the note:\n%s", md)
	}
	c := h.e.TemplateContext(h.ctx, r, res, res.Nodes["work"])
	prompt, _ := engine.RenderText(res.Nodes["work"].Prompt, c)
	if prompt != "Do the work. Reviewer said: Use the existing slug helper instead." {
		t.Errorf("prompt %q", prompt)
	}

	h.finish(id, "done", nil)
	gate, _ = h.st.LastVisit(h.ctx, id)
	if err := h.e.Decide(h.ctx, id, gate.Seq, "approve", "casey", ""); err != nil {
		t.Fatal(err)
	}
	if gate, _ = h.st.GetVisit(h.ctx, id, gate.Seq); string(gate.Outputs) != "{}" {
		t.Errorf("a decision without a note stores no note: %s", gate.Outputs)
	}
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("status %s: %s", r.Status, r.Error)
	}
}
