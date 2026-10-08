package resolve

import (
	"strings"
	"testing"
	"time"
)

func TestActionOutcomesComeFromTheSpec(t *testing.T) {
	r, issues := validateYAML(t, `  start: ci
  nodes:
    ci:
      type: action
      action: wait_for_checks
      with: { ref: main, branch: x }
      next: { passed: ship, failed: $fail, none: $fail, timeout: $fail }
    ship:
      type: action
      action: merge_pull_request
      outcomes: [merged, exploded]
      next: { merged: $success, exploded: $fail }
`)
	all := strings.Join(issues, "\n")
	for _, want := range []string{
		`node ship.outcomes: "exploded" is not an outcome of merge_pull_request (merged, conflict, blocked, timeout)`,
		`node ship.outcomes: merge_pull_request can emit "conflict": declare and route it`,
		`warning: node ci.with.branch: declared but not enforced: wait_for_checks ignores "branch"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "node ci.next") || strings.Contains(all, "node ci.outcomes") {
		t.Errorf("ci routes every outcome and should be clean:\n%s", all)
	}
	if got := r.Nodes["ci"].Timeout.Duration; got != 45*time.Minute {
		t.Errorf("waiting actions get their default timeout, got %s", got)
	}
}
