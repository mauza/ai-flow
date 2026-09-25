package resolve

import (
	"os"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func loadFlow(t *testing.T, path string) *flow.Flow {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := flow.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGoodFlowValidates(t *testing.T) {
	cfg := loadCfg(t)
	r := Resolve(loadFlow(t, "testdata/good.yaml"), cfg)
	for _, i := range Validate(r, cfg) {
		t.Errorf("unexpected issue: %s", i)
	}
	fix := r.Nodes["fix"]
	if fix.Type != flow.TypeAgent || fix.Harness != "pi" || fix.Runtime != "agent-base" {
		t.Errorf("preset/defaults not applied: %+v", fix.Node)
	}
	if fix.Model != "qwen-local" || len(fix.LLM.Fallbacks) != 1 || fix.LLM.Fallbacks[0] != "gemma-local" {
		t.Errorf("model defaults not applied: %+v", fix.LLM)
	}
	if got := fix.LLM.OnLimit["rate_limited"]; got == nil || got.Then != "fallback" {
		t.Errorf("model on_limit not inherited: %+v", got)
	}
	if fix.LLM.Limits == nil || fix.LLM.Limits.Tokens != 400000 {
		t.Errorf("node limits should win over catalog defaults: %+v", fix.LLM.Limits)
	}
	if l := r.Nodes["reproduce"].LLM.Limits; l == nil || l.Tokens != 500000 || l.Turns != 80 {
		t.Errorf("catalog default limits not applied: %+v", l)
	}
	if !contains(fix.Outcomes, "limit") {
		t.Errorf("limit outcome not added: %v", fix.Outcomes)
	}
	if !contains(r.Nodes["ask_human"].Outcomes, "timeout") {
		t.Errorf("gate timeout outcome missing")
	}
	if got := r.Order[0]; got != "reproduce" {
		t.Errorf("order starts with %s", got)
	}
}

func TestValidationErrors(t *testing.T) {
	cfg := loadCfg(t)
	base, _ := os.ReadFile("testdata/good.yaml")
	cases := []struct {
		name, from, to, want string
	}{
		{"unknown target", "next: { pass: size_check, fail: fix }", "next: { pass: nowhere, fail: fix }", `unknown node "nowhere"`},
		{"missing transition", "next: { pass: size_check, fail: fix }", "next: { pass: size_check }", `outcome "fail" has no transition`},
		{"unknown model", "model: gemma-local\n      grants: [\"repo/ai-flow-sandbox:read\"]", "model: gpt-9\n      grants: [\"repo/ai-flow-sandbox:read\"]", `unknown model "gpt-9"`},
		{"unbounded loop", "      max_visits: 3\n      on_exhausted: escalate\n", "", "unbounded loop"},
		{"bad cel", "run.diff.files_changed > 15", "run.diff.files_changed >", "CEL"},
		{"grant not allowed", `grants: ["repo/ai-flow-sandbox:write"]
      max_visits: 2`, `grants: ["repo/other:write"]
      max_visits: 2`, `unknown grant "repo/other"`},
		{"bad ref", "${{ nodes.reproduce.outputs.evidence }}", "${{ nodez.reproduce }}", `unknown root "nodez"`},
		{"unknown preset", "preset/code-review", "preset/nope", `unknown preset`},
		{"stray next", "next: { done: $success }", "next: { done: $success, oops: $fail }", `"oops" is not one of`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := string(base)
			if !strings.Contains(src, c.from) {
				t.Fatalf("fixture does not contain %q", c.from)
			}
			f, err := flow.Parse([]byte(strings.Replace(src, c.from, c.to, 1)))
			if err != nil {
				t.Fatal(err)
			}
			issues := Validate(Resolve(f, cfg), cfg)
			for _, i := range issues {
				if i.Severity == Error && strings.Contains(i.String(), c.want) {
					return
				}
			}
			t.Errorf("want error containing %q, got %v", c.want, issues)
		})
	}
}

func TestEvalCEL(t *testing.T) {
	ok, err := EvalCEL("run.diff.files_changed > 15", map[string]any{
		"run": map[string]any{"diff": map[string]any{"files_changed": float64(20)}},
	})
	if err != nil || !ok {
		t.Fatalf("got %v %v", ok, err)
	}
}
