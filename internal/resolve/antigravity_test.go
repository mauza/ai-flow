package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

func antigravityTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env.LLM.Upstreams["antigravity"] = config.Upstream{Type: config.UpstreamAntigravity, AuthEnv: "ANTIGRAVITY_OAUTH_TOKEN"}
	cfg.Catalog.Models["gemini-4-neon-high"] = &config.Model{
		Upstream:       "antigravity",
		Model:          "gemini-4-neon-high",
		Size:           "frontier",
		ContextTokens:  400000,
		Reasoning:      true,
		ThinkingFormat: "reasoning_effort",
	}
	cfg.Projects["sandbox"].Spec.Allow.Models = append(cfg.Projects["sandbox"].Spec.Allow.Models, "gemini-*")
	return cfg
}

func TestAntigravityHarnessAutoSelectAndValidation(t *testing.T) {
	cfg := antigravityTestConfig(t)

	// Agent node picking an antigravity model without specifying harness auto-selects antigravity.
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: agy-auto, project: sandbox}
spec:
  start: work
  nodes:
    work:
      type: agent
      model: gemini-4-neon-high
      prompt: fix the bug
      outcomes: [done]
      next: {done: classify}
    classify:
      type: llm
      model: gemini-4-neon-high
      prompt: classify
      outcomes: [ok]
      next: {ok: $success}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Resolve(f, cfg)
	if r.Nodes["work"].Harness != "antigravity" {
		t.Fatalf("expected harness antigravity, got %q", r.Nodes["work"].Harness)
	}
	if issues := Validate(r, cfg); HasErrors(issues) {
		t.Fatalf("unexpected validation errors: %v", issues)
	}

	// Explicitly forcing pi on an antigravity model must fail validation.
	badPi, _ := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: bad-pi, project: sandbox}
spec:
  start: work
  nodes:
    work:
      type: agent
      harness: pi
      model: gemini-4-neon-high
      prompt: fix
      outcomes: [done]
      next: {done: $success}
`))
	if issues := Validate(Resolve(badPi, cfg), cfg); !HasErrors(issues) || !strings.Contains(issues[0].String(), `requires harness "antigravity"`) {
		t.Fatalf("expected harness mismatch error, got %v", issues)
	}

	// Forcing antigravity harness on an OpenAI/LiteLLM model must fail validation.
	badAgy, _ := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: bad-agy, project: sandbox}
spec:
  start: work
  nodes:
    work:
      type: agent
      harness: antigravity
      model: ` + cfg.Catalog.Planner.Model + `
      prompt: fix
      outcomes: [done]
      next: {done: $success}
`))
	if issues := Validate(Resolve(badAgy, cfg), cfg); !HasErrors(issues) || !strings.Contains(issues[0].String(), `requires a model on an antigravity upstream`) {
		t.Fatalf("expected upstream mismatch error, got %v", issues)
	}
}
