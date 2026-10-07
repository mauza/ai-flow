package engine_test

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

const historyFlow = header + `  start: fix
  nodes:
    fix:
      type: agent
      model: ` + testModel + `
      prompt: "Fix it. All review rounds: ${{ nodes.review.history ?? \"none yet\" }}"
      outcomes: [done]
      max_visits: 4
      next: { done: review }
    review:
      type: llm
      model: ` + testModel + `
      prompt: review
      outputs: { comments: [string] }
      outcomes: [approve, changes]
      next: { approve: $success, changes: rounds }
    rounds:
      type: switch
      cases: [{ when: "size(nodes.review.history) >= 3", outcome: enough }]
      default: again
      next: { enough: $fail, again: fix }
`

func TestLoopHistoryReachesTemplatesCELAndContext(t *testing.T) {
	h := newHarness(t)
	id := h.flow(historyFlow)
	h.finish(id, "done", nil)
	h.finish(id, "changes", map[string]any{"comments": []string{"handle empty input"}})
	h.finish(id, "done", nil)
	h.finish(id, "changes", map[string]any{"comments": []string{"keep the old error message"}})

	r := h.run(id)
	if r.Status != store.RunRunning || r.CurrentNode != "fix" {
		t.Fatalf("want a third fix, got %s at %s (%s)", r.Status, r.CurrentNode, h.path(id))
	}
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	fix := res.Nodes["fix"]
	prompt, _ := engine.RenderText(fix.Prompt, h.e.TemplateContext(h.ctx, r, res, fix))
	if !strings.Contains(prompt, "handle empty input") || !strings.Contains(prompt, "keep the old error message") {
		t.Errorf("template history lacks a round:\n%s", prompt)
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	md := h.e.ContextMarkdown(h.ctx, r, res, fix, v.Seq)
	for _, want := range []string{"## Loop history", "review#1 → changes", `{"comments":["handle empty input"]}`, "review#2 → changes", "fix#2 → done"} {
		if !strings.Contains(md, want) {
			t.Errorf("context missing %q:\n%s", want, md)
		}
	}

	h.finish(id, "done", nil)
	h.finish(id, "changes", map[string]any{"comments": []string{"still wrong"}})
	if r := h.run(id); r.Status != store.RunFailed || !strings.Contains(r.Error, "rounds → enough") {
		t.Fatalf("CEL on history should stop after three rounds: %s %q (%s)", r.Status, r.Error, h.path(id))
	}
}

func TestNoLoopHistoryWithoutRepeats(t *testing.T) {
	h := newHarness(t)
	id := h.flow(historyFlow)
	h.finish(id, "done", nil)
	r := h.run(id)
	res, _ := h.e.Resolved(h.ctx, r)
	v, _ := h.st.LastVisit(h.ctx, id)
	if md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["review"], v.Seq); strings.Contains(md, "Loop history") {
		t.Errorf("a straight path has no loop history:\n%s", md)
	}
}
