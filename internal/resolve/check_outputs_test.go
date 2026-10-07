package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestCheckNodeOutputs(t *testing.T) {
	cfg := loadCfg(t)
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: check-outputs, project: sandbox }
spec:
  start: test
  nodes:
    test:
      type: check
      run: ./run-tests.sh
      outputs: { failed: integer, coverage: number, broken: tuple }
      next: { pass: route, fail: route }
    route:
      type: switch
      cases:
        - when: "nodes.test.outputs.failed == 0 && nodes.test.outputs.exit_code == 0"
          outcome: green
      default: red
      next: { green: $success, red: report }
    report:
      type: llm
      model: gpt-6.1-sol
      prompt: "Failures: ${{ nodes.test.outputs.failed }} of ${{ nodes.test.outputs.total }}. ${{ nodes.test.outputs.log_tail }}"
      outcomes: [done]
      next: { done: $fail }
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range Validate(Resolve(f, cfg), cfg) {
		got = append(got, i.String())
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		`error: node test.outputs.broken: unknown type "tuple"`,
		`warning: node report.prompt: ${{ nodes.test.outputs.total }}: node "test" declares no output "total"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "declared but not enforced") || strings.Contains(all, "outputs.log_tail") || strings.Contains(all, `"failed"`) {
		t.Errorf("declared check outputs and implicit exit_code/log_tail must be accepted:\n%s", all)
	}
}
