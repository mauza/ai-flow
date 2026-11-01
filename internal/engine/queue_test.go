package engine_test

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/store"
)

// After an audit, findings are fixed one per visit until none remain, then
// verification runs once.
func TestFindingsWorkQueue(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: audit
  nodes:
    audit:
      type: llm
      model: ` + testModel + `
      prompt: audit the change
      outcomes: [findings, clean]
      outputs: { findings: [string] }
      next: { findings: fix, clean: $success }
    fix:
      uses: preset/fix-next-finding
      model: ` + testModel + `
      grants: [repo/ai-flow-sandbox:write]
      next: { fixed: queue, rebutted: queue, stuck: $fail }
    queue:
      type: switch
      cases:
        - when: 'nodes.fix.outputs.remaining > 0'
          outcome: more
      default: done
      next: { more: fix, done: verify }
    verify:
      type: check
      run: make test
      next: { pass: $success, fail: $fail }
`)
	h.finish(id, "findings", map[string]any{"findings": []string{"a", "b", "c"}})
	for i, left := range []int{2, 1, 0} {
		h.finish(id, "fixed", map[string]any{"finding": string(rune('a' + i)), "remaining": left, "open": []string{}, "verification": []string{}})
	}
	h.finish(id, "pass", nil)
	if r := h.run(id); r.Status != store.RunSucceeded {
		t.Fatalf("status %s", r.Status)
	}
	if got := h.path(id); strings.Count(got, "fix:fixed") != 3 || !strings.HasSuffix(got, "queue:done verify:pass") {
		t.Fatalf("path %s", got)
	}
}
