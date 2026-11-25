package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

func TestQueryMetricsAction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		val := "0"
		if strings.Contains(q, "errors") {
			val = "0.25"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"%s"]}]}}`, val)
	}))
	defer srv.Close()

	h := newHarness(t)
	h.cfg.Env.Metrics.URL = srv.URL
	h.cfg.Projects["sandbox"].Spec.Observability = &config.Observability{
		Queries: []config.HealthQuery{
			{Name: "5xx rate", Query: "errors_total", Max: 0.05},
			{Name: "restarts", Query: "restarts_total", Max: 0},
		},
	}
	id := h.flow(header + `  start: check_metrics
  nodes:
    check_metrics:
      type: action
      action: query_metrics
      next: { ok: $fail, alert: $success, unavailable: $fail }
`)
	r := h.run(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("want alert→success, got %s: %s", r.Status, r.Error)
	}
	vs, _ := h.st.Visits(h.ctx, id)
	if len(vs) != 1 || vs[0].Outcome != "alert" || !strings.Contains(vs[0].Summary, "5xx rate") {
		t.Fatalf("unexpected visit: %+v", vs)
	}
}

func TestCreateTasksAndLedgerSuppression(t *testing.T) {
	h := newHarness(t)
	createdTitles := []string{}
	seenExt := map[string]string{}
	h.e.SetTaskCreator(func(ctx context.Context, c engine.ChildTask) (string, bool, error) {
		if id, dup := seenExt[c.Key]; dup {
			return id, false, nil
		}
		id := fmt.Sprintf("t-%d", len(createdTitles)+1)
		seenExt[c.Key] = id
		createdTitles = append(createdTitles, c.Title)
		return id, true, nil
	})
	// Pre-populate ledger with a rejected item so create_tasks skips it.
	must(t, h.e.UpsertLedger(h.ctx, "sandbox", engine.LedgerEntry{
		Key: "rewrite-everything", Title: "Rewrite everything", Status: engine.LedgerRejected,
	}))
	id := h.flow(header + `  start: spawn
  nodes:
    spawn:
      type: action
      action: create_tasks
      with:
        items: '[{"key":"rewrite-everything","title":"Rewrite everything","body":"nope"},{"key":"fix-nil-check","title":"Fix nil check","body":"In parser.go"},{"key":"add-timeout-test","title":"Add timeout test","body":"In client_test.go"}]'
        max: 2
      next: { created: $success, none: $fail }
`)
	r := h.run(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("want created→success, got %s: %s", r.Status, r.Error)
	}
	if len(createdTitles) != 2 || createdTitles[0] != "Fix nil check" || createdTitles[1] != "Add timeout test" {
		t.Fatalf("unexpected created titles: %v", createdTitles)
	}
	ledger := h.e.Ledger(h.ctx, "sandbox")
	if len(ledger) != 3 {
		t.Fatalf("expected 3 ledger entries (1 rejected + 2 proposed), got %d: %+v", len(ledger), ledger)
	}
}

// The catalog's audit template hands its keyed proposals to create_tasks.
func TestAuditTemplateSpawnsKeyedTasks(t *testing.T) {
	h := newHarness(t)
	var got []engine.ChildTask
	h.e.SetTaskCreator(func(_ context.Context, c engine.ChildTask) (string, bool, error) {
		got = append(got, c)
		return "t-child", true, nil
	})
	must(t, h.st.CreateTask(h.ctx, &store.Task{ID: "t-audit", Source: "schedule", Title: "Audit", Project: "sandbox"}))
	h.cfg.Env.Metrics.URL = ""
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", Project: "sandbox", TaskID: "t-audit", YAML: header + `  start: sweep
  nodes:
    sweep:
      uses: template/continuous-audit
      model: ` + testModel + `
      next: { created: $success, clean: $success }
`}); err != nil {
		t.Fatal(err)
	}
	r, err := h.e.CreateRun(h.ctx, "f", 0, "")
	must(t, err)
	for range 3 {
		h.e.Tick(h.ctx)
	}
	h.finish(r.ID, "proposed", map[string]any{"proposals": []any{
		map[string]any{"key": "parser-nil-deref", "title": "Guard \"nil\" input — parser", "body": "In parser.go\nrun go test"},
	}, "rationale": "", "project_map": "", "map_commit": ""})
	if r := h.run(r.ID); r.Status != store.RunSucceeded {
		t.Fatalf("%s: %s (%s)", r.Status, r.Error, h.path(r.ID))
	}
	if len(got) != 1 || got[0].Key != "parser-nil-deref" || got[0].Title != "Guard \"nil\" input — parser" || got[0].ParentTaskID != "t-audit" || !got[0].Plan {
		t.Fatalf("spawned %+v", got)
	}
}

func TestDeliveredWorkWaitsForItsPullRequest(t *testing.T) {
	for _, tc := range []struct{ source, pr, want string }{
		{"flow", "https://github.com/o/r/pull/1", engine.LedgerPROpen},
		{"storymap", "https://github.com/o/r/pull/1", engine.LedgerPROpen},
		{"flow", "", engine.LedgerShipped},
		{"schedule", "", ""},
	} {
		h := newHarness(t)
		must(t, h.st.CreateTask(h.ctx, &store.Task{ID: "t-1", Source: tc.source, Identifier: "card", Title: "Card", Project: "sandbox"}))
		if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", Project: "sandbox", TaskID: "t-1", YAML: header + `  start: step
  nodes:
    step: { type: check, run: make test, next: { pass: $success, fail: $fail } }
`}); err != nil {
			t.Fatal(err)
		}
		r, err := h.e.CreateRun(h.ctx, "f", 0, "")
		must(t, err)
		must(t, h.st.UpdateRun(h.ctx, r.ID, map[string]any{"pr_url": tc.pr}))
		h.e.Tick(h.ctx)
		h.finish(r.ID, "pass", nil)
		got := ""
		for _, it := range h.e.Ledger(h.ctx, "sandbox") {
			got = it.Status
		}
		if got != tc.want {
			t.Errorf("%s task with PR %q: ledger %q, want %q", tc.source, tc.pr, got, tc.want)
		}
	}
}

func TestCheckRatchetBlocksRegressionAndPromotesOnSuccess(t *testing.T) {
	h := newHarness(t)
	// Seed initial baseline: test_count >= 10, lint_warnings <= 2.
	must(t, h.e.SetBaselines(h.ctx, "sandbox", map[string]engine.BaselineEntry{
		"test_count":    {Value: 10, Direction: "min"},
		"lint_warnings": {Value: 2, Direction: "max"},
	}))

	flowYAML := header + `  start: verify
  nodes:
    verify:
      type: check
      run: make test
      outputs: { test_count: integer, lint_warnings: integer }
      ratchet: { test_count: min, lint_warnings: max }
      next: { pass: $success, fail: $fail }
`
	// Regression case: test_count dropped to 8 -> ratchet forces fail.
	r1 := h.flow(flowYAML)
	v1, _ := h.st.LastVisit(h.ctx, r1)
	raw1, _ := json.Marshal(map[string]any{"test_count": 8, "lint_warnings": 1})
	must(t, h.st.UpdateVisit(h.ctx, r1, v1.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": "pass", "outputs": string(raw1), "finished_at": store.Now()}))
	h.e.Tick(h.ctx)
	if got := h.run(r1); got.Status != store.RunFailed || !strings.Contains(got.Error, "ratchet regression") {
		t.Fatalf("expected ratchet regression failure, got %s: %q", got.Status, got.Error)
	}

	// Improvement case: test_count rose to 15, lint_warnings dropped to 0 -> succeeds and promotes baselines.
	r2 := h.flow(flowYAML)
	v2, _ := h.st.LastVisit(h.ctx, r2)
	raw2, _ := json.Marshal(map[string]any{"test_count": 15, "lint_warnings": 0})
	must(t, h.st.UpdateVisit(h.ctx, r2, v2.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": "pass", "outputs": string(raw2), "finished_at": store.Now()}))
	h.e.Tick(h.ctx)
	if got := h.run(r2); got.Status != store.RunSucceeded {
		t.Fatalf("expected ratchet improvement success, got %s: %q", got.Status, got.Error)
	}
	b := h.e.Baselines(h.ctx, "sandbox")
	if b["test_count"].Value != 15 || b["lint_warnings"].Value != 0 {
		t.Fatalf("baselines not promoted: %+v", b)
	}
}
