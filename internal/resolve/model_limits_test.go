package resolve

import (
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func TestModelLimitsBeatCatalogDefaultsPerField(t *testing.T) {
	cfg := loadCfg(t)
	model := cfg.Catalog.Planner.Model
	cfg.Catalog.Defaults.LLM.Limits = &flow.Limits{Tokens: 500000, Turns: 80}
	cfg.Catalog.Models[model].LLM = &flow.LLMConfig{Limits: &flow.Limits{Tokens: 3000000}}
	f, err := flow.Parse([]byte(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: m, project: sandbox }
spec:
  start: a
  nodes:
    a: { type: agent, model: ` + model + `, prompt: x, outcomes: [done], next: { done: b } }
    b: { type: agent, model: ` + model + `, prompt: x, llm: { limits: { tokens: 1000 } }, outcomes: [done], next: { done: $success } }
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Resolve(f, cfg)
	if l := r.Nodes["a"].LLM.Limits; l.Tokens != 3000000 || l.Turns != 80 {
		t.Errorf("model tokens should beat the default and keep its turn cap: %+v", l)
	}
	if l := r.Nodes["b"].LLM.Limits; l.Tokens != 1000 || l.Turns != 80 {
		t.Errorf("node limits beat the model: %+v", l)
	}
	// Resolving must not write into shared catalog config.
	if l := cfg.Catalog.Models[model].LLM.Limits; l.Turns != 0 || l.Tokens != 3000000 {
		t.Errorf("model config mutated: %+v", l)
	}
	if l := cfg.Catalog.Defaults.LLM.Limits; l.Tokens != 500000 || l.Turns != 80 {
		t.Errorf("defaults mutated: %+v", l)
	}
}
