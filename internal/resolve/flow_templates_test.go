package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestTemplateCallProblemsAreReported(t *testing.T) {
	cfg := loadCfg(t)
	for _, tc := range []struct{ call, extra, want string }{
		{"next: { created: $success }", "", `can end with "clean"`},
		{"next: { created: $success, clean: $success, oops: $fail }", "", "not an outcome of template/continuous-audit"},
		{"with: { max: 1 }, next: { created: $success, clean: $success }", "", "with: template/continuous-audit ignores it"},
		{"max_visits: 3, next: { created: $success, clean: $success }", "", "max_visits: template/continuous-audit ignores it"},
		{"next: { created: sweep_audit, clean: $success }", "    sweep_audit: { type: check, run: make, next: { pass: $success, fail: $fail } }\n", `adds a step "sweep_audit"`},
		{"next: { created: $success, clean: $success }", "", ""},
	} {
		f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: use-tpl, project: sandbox }
spec:
  start: sweep
  nodes:
    sweep: { uses: template/continuous-audit, model: gpt-6.1-sol, ` + tc.call + " }\n" + tc.extra))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, i := range Validate(Resolve(f, cfg), cfg) {
			got = append(got, i.String())
		}
		all := strings.Join(got, "\n")
		if (tc.want == "") != (all == "") || !strings.Contains(all, tc.want) {
			t.Errorf("%s: want %q, got %q", tc.call, tc.want, all)
		}
	}
}

func TestNodeRefsAreRemappedOnce(t *testing.T) {
	got := remapNodeRefs("nodes.a.outcome nodes.x_a.outcome nodes.other.outcome", map[string]string{"a": "x_a", "x_a": "x_x_a"})
	if got != "nodes.x_a.outcome nodes.x_x_a.outcome nodes.other.outcome" {
		t.Fatal(got)
	}
}

func TestFlowTemplateExpansion(t *testing.T) {
	cfg := loadCfg(t)
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: use-tpl, project: sandbox }
spec:
  start: sweep
  nodes:
    sweep:
      uses: template/continuous-audit
      model: gpt-6.1-sol
      next: { created: $success, clean: $success }
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Resolve(f, cfg)
	issues := Validate(r, cfg)
	if HasErrors(issues) {
		t.Fatalf("expanded template failed validation: %v", issues)
	}
	if r.Flow.Spec.Start != "sweep_metrics" {
		t.Fatalf("start=%q, want sweep_metrics", r.Flow.Spec.Start)
	}
	audit := r.Nodes["sweep_audit"]
	if audit == nil || audit.Model != "gpt-6.1-sol" || audit.Next["clean"] != flow.Success {
		t.Fatalf("unexpected expanded audit node: %+v", audit)
	}
	spawn := r.Nodes["sweep_spawn"]
	if spawn == nil || !strings.Contains(spawn.With["items"].(string), "nodes.sweep_audit.outputs.proposals") {
		t.Fatalf("template node refs not remapped: %+v", spawn)
	}
}

func TestUnknownTemplateValidationError(t *testing.T) {
	cfg := loadCfg(t)
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: bad-tpl, project: sandbox }
spec:
  start: x
  nodes:
    x:
      uses: template/does-not-exist
      next: { done: $success }
`))
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(Resolve(f, cfg), cfg)
	found := false
	for _, i := range issues {
		if i.Severity == Error && strings.Contains(i.Message, "unknown template") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown template error, got %v", issues)
	}
}
