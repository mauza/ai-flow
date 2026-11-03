package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

// A run review sees the instructions each step ran with (presets are only
// named in the flow YAML), from the run's pinned snapshot.
func TestRunRecordHasPinnedStepInstructions(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	a := &App{Cfg: cfg, Store: st, Hub: hub.New(), Engine: engine.New(cfg, st, nil, hub.New(), nil)}
	if _, err := st.SaveFlow(ctx, &store.FlowVersion{Name: "f", Project: "sandbox", YAML: `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: f, project: sandbox }
spec:
  start: review
  nodes:
    review:
      uses: preset/code-review
      model: gpt-6.1-sol
      next: { approve: $success, changes: $fail }
`}); err != nil {
		t.Fatal(err)
	}
	run, err := a.Engine.CreateRun(ctx, "f", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddVisit(ctx, run.ID, "review", "llm"); err != nil {
		t.Fatal(err)
	}
	pinned := cfg.Catalog.Presets["code-review"].Prompt
	cfg.Catalog.Presets["code-review"].Prompt = "changed after the run started"
	rec, err := a.runRecord(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec, "## Instructions the steps ran with") || !strings.Contains(rec, strings.TrimSpace(pinned)[:60]) {
		t.Fatalf("record lacks the pinned instructions:\n%s", rec)
	}
}
