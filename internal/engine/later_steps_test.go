package engine_test

import (
	"strings"
	"testing"
)

// A reviewer sees what the flow still checks after it, so it does not block
// on what only a later step (here a production check) can verify.
func TestContextListsLaterSteps(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: build it
      outcomes: [done]
      next: { done: review }
    review:
      type: llm
      model: ` + testModel + `
      prompt: review it
      outcomes: [approve, changes]
      max_visits: 2
      next: { approve: prod_check, changes: work }
    prod_check:
      type: check
      description: Fetch the live pages as a crawler and compare visit counters.
      run: ./check-prod.sh
      next: { pass: $success, fail: $fail }
`)
	h.finish(id, "done", nil)
	v, _ := h.st.LastVisit(h.ctx, id)
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["review"], v.Seq)
	later := md[strings.Index(md, "## Later in this flow"):]
	if !strings.Contains(later, "- prod_check (check): Fetch the live pages as a crawler") || strings.Contains(later, "- work ") {
		t.Fatalf("later steps:\n%s", md)
	}
}
