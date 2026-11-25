package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/store"
)

func TestImprovementHTTPAPI(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, _ := json.Marshal(map[string]any{
			"grades": []map[string]any{
				{"rubric": "efficiency", "pass": true, "reason": "clean"},
				{"rubric": "instruction_adherence", "pass": false, "blame": "agent", "theme": "missing-check", "target_kind": "preset", "target": "code-review", "reason": "skipped check"},
				{"rubric": "unmentioned_workarounds", "pass": true, "reason": "disclosed"},
			},
		})
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": string(out)}}},
		})
	}))
	defer upstream.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	cfg, _, err = app.LoadStoredConfig(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env.LLM.Upstreams["home"] = config.Upstream{BaseURL: upstream.URL}
	cfg.Catalog.Planner.Model = "gpt-6.1-sol"
	cfg.Catalog.Planner.Stream = false
	cfg.Catalog.Improvement.Model = "gpt-6.1-sol"

	h := hub.New()
	eng := engine.New(cfg, st, nil, h, nil)
	a := &app.App{Cfg: cfg, Store: st, Engine: eng, Hub: h, LLM: llm.New(cfg)}
	srv, err := New(a, nil, fstest.MapFS{"index.html": {Data: []byte("ok")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	_, err = st.SaveFlow(ctx, &store.FlowVersion{Name: "f-api", Project: "sandbox", YAML: `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: f-api, project: sandbox }
spec:
  start: review
  nodes:
    review:
      uses: preset/code-review
      model: gpt-6.1-sol
      next: { approve: $success, changes: $fail }
`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.CreateRun(ctx, "f-api", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunState(ctx, run, store.RunSucceeded, "", store.Now()); err != nil {
		t.Fatal(err)
	}

	// Sync grade via POST /api/runs/{id}/grade?sync=1
	resp, err := http.Post(ts.URL+"/api/runs/"+run.ID+"/grade?sync=1", "application/json", nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("POST grade: status=%v err=%v", resp, err)
	}
	resp.Body.Close()

	// GET /api/runs/{id}/grades
	resp, err = http.Get(ts.URL + "/api/runs/" + run.ID + "/grades")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET run grades: %v", err)
	}
	var gv app.RunGradesView
	json.NewDecoder(resp.Body).Decode(&gv)
	resp.Body.Close()
	if len(gv.Grades) != 4 {
		t.Fatalf("expected 4 grades, got %d", len(gv.Grades))
	}

	// POST /api/improvement/grades/{id}/disagree
	var failID string
	for _, g := range gv.Grades {
		if !g.Pass {
			failID = g.ID
		}
	}
	resp, err = http.Post(ts.URL+"/api/improvement/grades/"+failID+"/disagree", "application/json", bytes.NewBufferString(`{"disagreed":true,"by":"casey","note":"disagreed"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("POST disagree: %v", err)
	}
	resp.Body.Close()

	// GET /api/improvement/summary
	resp, err = http.Get(ts.URL + "/api/improvement/summary")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET summary: %v", err)
	}
	var sum app.ImprovementSummary
	json.NewDecoder(resp.Body).Decode(&sum)
	resp.Body.Close()
	if sum.Stats.GradedRuns != 1 || sum.Stats.DisagreedGrades != 1 {
		t.Fatalf("unexpected summary stats: %+v", sum.Stats)
	}
}
