package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestDeclaredButNotEnforcedWarnings(t *testing.T) {
	cfg := loadCfg(t)
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: ignored, project: sandbox }
spec:
  budget: { usd: 1, wall: 2h }
  start: classify
  nodes:
    classify:
      type: llm
      model: gpt-6-sol
      llm: { thinking: high }
      skills: [small-diffs]
      prompt: classify
      outcomes: [done]
      next: { done: test }
    test:
      type: check
      run: make test
      prompt: this is never shown to anything
      inputs: { x: "${{ task.title }}" }
      next: { pass: route, fail: $fail }
    route:
      type: switch
      model: gpt-6-sol
      timeout: 5m
      cases: [{ when: "run.diff.files_changed > 1", outcome: big }]
      default: small
      next: { big: $success, small: $success }
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range Validate(Resolve(f, cfg), cfg) {
		if i.Severity == Error {
			t.Errorf("unexpected error: %s", i)
		}
		if strings.Contains(i.Message, "declared but not enforced") {
			got = append(got, i.Node+"."+i.Field)
		}
	}
	want := []string{".spec.budget.wall", "classify.skills", "classify.llm.thinking", "test.inputs", "test.prompt", "route.model", "route.timeout"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("warnings\n got %v\nwant %v", got, want)
	}
}

func TestPresetFieldsNeverTriggerIgnoredWarnings(t *testing.T) {
	cfg := loadCfg(t)
	r := Resolve(loadFlow(t, "testdata/good.yaml"), cfg)
	for _, i := range Validate(r, cfg) {
		if strings.Contains(i.Message, "declared but not enforced") {
			t.Errorf("unexpected: %s", i)
		}
	}
}
