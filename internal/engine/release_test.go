package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

// fakeWorld stands in for GitHub, the deployed app and VictoriaMetrics.
type fakeWorld struct {
	mu        sync.Mutex
	mergeable any    // nil, true or false
	state     string // mergeable_state
	merged    bool
	merges    int
	checks    []map[string]any // check runs
	version   string
	metric    string // value returned by every PromQL query; "" = query error
	appStatus int

	dispatches  []map[string]any // rollback workflow dispatch bodies
	rollbackRun map[string]any   // the dispatched run, once GitHub lists it
}

func (w *fakeWorld) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := r.URL.Path
		reply := func(v any) { json.NewEncoder(rw).Encode(v) }
		switch {
		case p == "/repos/your-org/ai-flow-sandbox/pulls/7" && r.Method == "GET":
			reply(map[string]any{"number": 7, "state": "open", "merged": w.merged, "mergeable": w.mergeable, "mergeable_state": w.state,
				"merge_commit_sha": "mergedsha0000001", "head": map[string]any{"sha": "headsha"}})
		case p == "/repos/your-org/ai-flow-sandbox/pulls/7/merge" && r.Method == "PUT":
			w.merges++
			w.merged = true
			reply(map[string]any{"sha": "mergedsha0000001", "merged": true})
		case p == "/repos/your-org/ai-flow-sandbox/commits/mergedsha0000001":
			reply(map[string]any{"sha": "mergedsha0000001", "parents": []any{map[string]any{"sha": "oldsha0000000"}, map[string]any{"sha": "headsha"}}})
		case p == "/repos/your-org/ai-flow-sandbox/actions/workflows/rollback.yml/dispatches" && r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			w.dispatches = append(w.dispatches, body)
			rw.WriteHeader(204)
		case p == "/repos/your-org/ai-flow-sandbox/actions/workflows/rollback.yml/runs":
			runs := []any{map[string]any{"id": 1, "status": "completed", "conclusion": "success", "created_at": "2020-01-01T00:00:00Z"}}
			if w.rollbackRun != nil {
				runs = append(runs, w.rollbackRun)
			}
			reply(map[string]any{"workflow_runs": runs})
		case p == "/repos/your-org/ai-flow-sandbox/actions/runs/42":
			reply(w.rollbackRun)
		case strings.HasPrefix(p, "/repos/your-org/ai-flow-sandbox/branches/"):
			reply(map[string]any{"commit": map[string]any{"sha": "headsha"}})
		case strings.HasSuffix(p, "/check-runs"):
			reply(map[string]any{"check_runs": w.checks})
		case strings.HasSuffix(p, "/status"):
			reply(map[string]any{"statuses": []any{}})
		case p == "/version":
			reply(map[string]any{"commit": w.version})
		case p == "/app":
			rw.WriteHeader(w.appStatus)
		case p == "/api/v1/query":
			if w.metric == "" {
				reply(map[string]any{"status": "error", "error": "vmsingle unavailable"})
				return
			}
			reply(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector",
				"result": []any{map[string]any{"value": []any{1.0, w.metric}}}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, p)
			rw.WriteHeader(404)
		}
	})
}

func (w *fakeWorld) set(f func(*fakeWorld)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

func releaseHarness(t *testing.T, w *fakeWorld, soak string) (*harness, string) {
	t.Helper()
	return releaseFlow(t, w, nil, `    ship:
      type: action
      action: merge_pull_request
      next: { merged: live, conflict: $fail, blocked: $fail, timeout: $fail }
    live:
      type: action
      action: wait_for_deploy
      next: { deployed: soak, timeout: $fail }
    soak:
      type: action
      action: check_health
      with: { duration: `+soak+` }
      next: { healthy: $success, degraded: $fail, timeout: $fail }
`)
}

// releaseFlow runs a flow that starts with a passing-CI gate (ci → ship) and
// continues with the given nodes, against the fake world.
func releaseFlow(t *testing.T, w *fakeWorld, deploy func(*config.Deploy), nodes string) (*harness, string) {
	t.Helper()
	srv := httptest.NewServer(w.handler(t))
	t.Cleanup(srv.Close)
	h := newHarness(t)
	h.e = engine.New(h.cfg, h.st, h.l, hub.New(), github.New(srv.URL, "t"))
	h.e.OrphanGrace, h.e.PollInterval = 0, 0
	h.cfg.Env.Metrics.URL = srv.URL
	h.cfg.Projects["sandbox"].Spec.Deploy = &config.Deploy{
		Branch: "main", URL: srv.URL + "/app",
		Versions: []config.VersionProbe{{URL: srv.URL + "/version", Field: "commit"}},
		Health:   []config.HealthQuery{{Name: "5xx rate", Query: `sum(rate(x[5m]))`, Max: 0.05}},
	}
	if deploy != nil {
		deploy(h.cfg.Projects["sandbox"].Spec.Deploy)
	}
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", Project: "sandbox", YAML: header + `  start: ci
  nodes:
    ci:
      type: action
      action: wait_for_checks
      with: { settle: 1h }
      next: { passed: ship, failed: $fail, none: $fail, timeout: $fail }
` + nodes}); err != nil {
		t.Fatal(err)
	}
	r, err := h.e.CreateRun(h.ctx, "f", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// The PR is opened before CI in a real flow; attach it before the first tick.
	if err := h.st.UpdateRun(h.ctx, r.ID, map[string]any{"pr_url": "https://github.com/mauza/ai-flow-sandbox/pull/7"}); err != nil {
		t.Fatal(err)
	}
	return h, r.ID
}

func (h *harness) current(id string) *store.Visit {
	h.t.Helper()
	v, err := h.st.LastVisit(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func TestReleasePipelineShipsAHealthyDeploy(t *testing.T) {
	w := &fakeWorld{state: "clean", version: "oldsha0000000", metric: "0", appStatus: 200,
		checks: []map[string]any{{"name": "test", "status": "in_progress"}}}
	h, id := releaseHarness(t, w, "300ms")

	h.e.Tick(h.ctx)
	if v := h.current(id); v.Node != "ci" || v.Status != store.VisitRunning || !strings.Contains(v.Progress, "0/1 checks finished") {
		t.Fatalf("ci should wait: %+v", v)
	}
	w.set(func(w *fakeWorld) { w.checks[0]["status"], w.checks[0]["conclusion"] = "completed", "success" })
	h.e.Tick(h.ctx) // ci passes → ship: GitHub still computing mergeability
	if v := h.current(id); v.Node != "ship" || v.Status != store.VisitRunning {
		t.Fatalf("ship should wait for mergeability: %+v", v)
	}
	w.set(func(w *fakeWorld) { w.mergeable = true })
	h.e.Tick(h.ctx) // merged → live waits for the new version
	if v := h.current(id); v.Node != "live" || !strings.Contains(v.Progress, "reports oldsha0") {
		t.Fatalf("live should wait for mergeds: %+v (%s)", v, h.path(id))
	}
	w.set(func(w *fakeWorld) { w.version = "MERGEDSHA0000001" })
	h.e.Tick(h.ctx) // deployed → soak starts
	if v := h.current(id); v.Node != "soak" || !strings.Contains(v.Progress, "Soaking 0s/300ms: all checks ok") {
		t.Fatalf("soak progress must start at zero: %+v", v)
	}
	h.e.Tick(h.ctx)
	time.Sleep(350 * time.Millisecond)
	h.e.Tick(h.ctx)
	r := h.run(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("want success, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}
	if w.merges != 1 {
		t.Errorf("merged %d times", w.merges)
	}
	vs, _ := h.st.Visits(h.ctx, id)
	var merge struct{ SHA string }
	json.Unmarshal(vs[1].Outputs, &merge)
	if merge.SHA != "mergedsha0000001" {
		t.Errorf("merge outputs %s", vs[1].Outputs)
	}
}

func TestReleasePipelineDegradesOnSustainedViolation(t *testing.T) {
	w := &fakeWorld{state: "clean", mergeable: true, version: "mergedsha0000001", metric: "0.5", appStatus: 200,
		checks: []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}}}
	h, id := releaseHarness(t, w, "1h")
	for i := 0; i < 6 && !store.RunDone(h.run(id).Status); i++ {
		h.e.Tick(h.ctx)
	}
	r := h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(h.path(id), "soak:degraded") {
		t.Fatalf("want degraded, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}
	v := h.current(id)
	if !strings.Contains(string(v.Outputs), `5xx rate: 0.5 (max 0.05)`) {
		t.Errorf("violations: %s", v.Outputs)
	}
}

func TestHealthQueryErrorsAreInconclusiveNotDegraded(t *testing.T) {
	w := &fakeWorld{state: "clean", mergeable: true, version: "mergedsha0000001", metric: "", appStatus: 200,
		checks: []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}}}
	h, id := releaseHarness(t, w, "1ms")
	for i := 0; i < 8; i++ {
		h.e.Tick(h.ctx)
	}
	v := h.current(id)
	if v.Node != "soak" || v.Status != store.VisitRunning || !strings.Contains(v.Progress, "inconclusive 5xx rate: vmsingle unavailable") {
		t.Fatalf("a metrics outage must keep soaking, not judge: %+v (%s)", v, h.path(id))
	}
	h.st.UpdateVisit(h.ctx, id, v.Seq, map[string]any{"deadline": store.Now() - 1})
	h.e.Tick(h.ctx)
	if !strings.HasSuffix(h.path(id), "soak:timeout") {
		t.Fatalf("an unconfirmable soak times out: %s", h.path(id))
	}
}

func TestMergeOutcomesAndCIOutcomes(t *testing.T) {
	for _, c := range []struct {
		name  string
		world *fakeWorld
		want  string
	}{
		{"conflict", &fakeWorld{mergeable: false, state: "dirty", checks: []map[string]any{{"name": "t", "status": "completed", "conclusion": "success"}}}, "ship:conflict"},
		{"blocked", &fakeWorld{mergeable: true, state: "blocked", checks: []map[string]any{{"name": "t", "status": "completed", "conclusion": "success"}}}, "ship:blocked"},
		{"ci failed", &fakeWorld{checks: []map[string]any{{"name": "lint", "status": "completed", "conclusion": "failure"}, {"name": "t", "status": "completed", "conclusion": "success"}}}, "ci:failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, id := releaseHarness(t, c.world, "1h")
			for i := 0; i < 4 && !store.RunDone(h.run(id).Status); i++ {
				h.e.Tick(h.ctx)
			}
			if got := h.path(id); !strings.HasSuffix(got, c.want) {
				t.Fatalf("path %s, want ...%s", got, c.want)
			}
			if c.name == "ci failed" && !strings.Contains(string(h.current(id).Outputs), `"failed":["lint"]`) {
				t.Errorf("outputs %s", h.current(id).Outputs)
			}
		})
	}
}

func TestNoCIReportedWithinSettleIsNone(t *testing.T) {
	w := &fakeWorld{}
	h, id := releaseHarness(t, w, "1h")
	h.e.Tick(h.ctx)
	v := h.current(id)
	if v.Status != store.VisitRunning {
		t.Fatalf("%+v", v)
	}
	// Pretend the visit started long ago: settle (1h in the flow) has passed.
	h.st.UpdateVisit(h.ctx, id, v.Seq, map[string]any{"started_at": store.Now() - 2*time.Hour.Milliseconds()})
	h.e.Tick(h.ctx)
	if got := h.path(id); got != "ci:none" {
		t.Fatalf("path %s", got)
	}
	if !strings.Contains(h.current(id).Summary, fmt.Sprintf("No CI reported on %s", "headsha")) {
		t.Errorf("summary %q", h.current(id).Summary)
	}
}

const rollbackNodes = `    ship:
      type: action
      action: merge_pull_request
      next: { merged: live, conflict: $fail, blocked: $fail, timeout: $fail }
    live:
      type: action
      action: wait_for_deploy
      next: { deployed: soak, timeout: rollback }
    soak:
      type: action
      action: check_health
      with: { duration: 1h }
      next: { healthy: $success, degraded: rollback, timeout: rollback }
    rollback:
      type: action
      action: rollback_deploy
      next: { rolled_back: restored, failed: $fail, timeout: $fail }
    restored:
      type: action
      action: wait_for_deploy
      with: { expect: "${{ run.previous_sha }}" }
      next: { deployed: $fail, timeout: $fail }
`

func TestFastRollbackRestoresThePreviousRelease(t *testing.T) {
	w := &fakeWorld{state: "clean", mergeable: true, version: "oldsha0000000", metric: "0.5", appStatus: 200,
		checks: []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}}}
	h, id := releaseFlow(t, w, func(d *config.Deploy) {
		d.Rollback = &config.RollbackSpec{Workflow: "rollback.yml"}
		d.ArgoCDApp, d.ArgoCDNamespace = "sandbox-app", "argocd"
	}, rollbackNodes)
	var refreshes []string
	h.e.SetArgoRefresher(func(_ context.Context, ns, app string) error {
		refreshes = append(refreshes, ns+"/"+app)
		return nil
	})

	h.e.Tick(h.ctx) // ci → ship merges → live waits (and asks Argo to refresh)
	h.e.Tick(h.ctx)
	if v := h.current(id); v.Node != "live" || v.Status != store.VisitRunning {
		t.Fatalf("live should wait: %+v (%s)", v, h.path(id))
	}
	if len(refreshes) == 0 || refreshes[0] != "argocd/sandbox-app" {
		t.Fatalf("argo refreshes %v", refreshes)
	}
	vs, _ := h.st.Visits(h.ctx, id)
	if !strings.Contains(string(vs[1].Outputs), `"previous_sha":"oldsha0000000"`) {
		t.Fatalf("merge outputs %s", vs[1].Outputs)
	}

	w.set(func(w *fakeWorld) { w.version = "mergedsha0000001" })
	for i := 0; i < 8 && len(w.dispatches) == 0; i++ {
		h.e.Tick(h.ctx) // deployed → soak degrades → rollback dispatches
	}
	if len(w.dispatches) != 1 {
		t.Fatalf("dispatches %v (%s)", w.dispatches, h.path(id))
	}
	if inputs, _ := w.dispatches[0]["inputs"].(map[string]any); w.dispatches[0]["ref"] != "main" || inputs["sha"] != "mergedsha0000001" {
		t.Errorf("dispatch %v", w.dispatches[0])
	}
	h.e.Tick(h.ctx) // GitHub has not listed the run yet: keep waiting, never re-dispatch
	if v := h.current(id); v.Node != "rollback" || !strings.Contains(v.Progress, "Waiting for GitHub to start") {
		t.Fatalf("%+v", v)
	}
	w.set(func(w *fakeWorld) {
		w.rollbackRun = map[string]any{"id": 42, "status": "in_progress", "html_url": "https://gh/run/42", "created_at": time.Now().UTC().Format(time.RFC3339)}
	})
	h.e.Tick(h.ctx)
	if v := h.current(id); v.Node != "rollback" || !strings.Contains(v.Progress, "in progress") {
		t.Fatalf("%+v", v)
	}
	w.set(func(w *fakeWorld) { w.rollbackRun["status"], w.rollbackRun["conclusion"] = "completed", "success" })
	h.e.Tick(h.ctx) // rolled back → restored waits for the previous commit
	h.e.Tick(h.ctx)
	if v := h.current(id); v.Node != "restored" || !strings.Contains(v.Progress, "Waiting for oldsha0") {
		t.Fatalf("restored should wait for the previous release: %+v (%s)", v, h.path(id))
	}
	w.set(func(w *fakeWorld) { w.version = "oldsha0000000" })
	h.e.Tick(h.ctx)
	if got := h.path(id); !strings.HasSuffix(got, "soak:degraded rollback:rolled_back restored:deployed") {
		t.Fatalf("path %s", got)
	}
	if len(w.dispatches) != 1 {
		t.Errorf("dispatched %d times", len(w.dispatches))
	}
}

func TestFailedRollbackWorkflowIsFailed(t *testing.T) {
	w := &fakeWorld{state: "clean", mergeable: true, version: "mergedsha0000001", metric: "0.5", appStatus: 200,
		checks:      []map[string]any{{"name": "test", "status": "completed", "conclusion": "success"}},
		rollbackRun: map[string]any{"id": 42, "status": "completed", "conclusion": "failure", "html_url": "https://gh/run/42", "created_at": time.Now().UTC().Add(time.Minute).Format(time.RFC3339)}}
	h, id := releaseFlow(t, w, func(d *config.Deploy) { d.Rollback = &config.RollbackSpec{Workflow: "rollback.yml"} }, rollbackNodes)
	for i := 0; i < 10 && !store.RunDone(h.run(id).Status); i++ {
		h.e.Tick(h.ctx)
	}
	if got := h.path(id); !strings.HasSuffix(got, "rollback:failed") {
		t.Fatalf("path %s", got)
	}
	if !strings.Contains(h.current(id).Summary, "rollback.yml ended failure: https://gh/run/42") {
		t.Errorf("summary %q", h.current(id).Summary)
	}
}
