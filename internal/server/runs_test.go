package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

type stubLauncher struct {
	mu    sync.Mutex
	state map[string]engine.JobState
}

func (l *stubLauncher) Launch(_ context.Context, s engine.LaunchSpec) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := engine.JobName(s)
	l.state[name] = engine.JobRunning
	return name, nil
}

func (l *stubLauncher) Status(_ context.Context, name string) (engine.JobStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, ok := l.state[name]; ok {
		return engine.JobStatus{State: st}, nil
	}
	return engine.JobStatus{State: engine.JobMissing}, nil
}

func (l *stubLauncher) Kill(_ context.Context, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state[name] = engine.JobFailed
	return nil
}

func call(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const routeFlow = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: routes, project: sandbox }
spec:
  start: fix
  nodes:
    fix:
      type: agent
      model: gpt-6.1-sol
      prompt: fix it
      outcomes: [done]
      next: { done: approve }
    approve:
      type: gate
      prompt: ship it?
      outcomes: [ship, hold]
      next: { ship: $success, hold: $fail }
`

// The run lifecycle through the HTTP API: save a flow, start, read, cancel
// and resume a run, and decide a gate.
func TestRunRoutes(t *testing.T) {
	s := testServer(t, "")
	s.app.Engine = engine.New(s.app.Cfg, s.app.Store, &stubLauncher{state: map[string]engine.JobState{}}, s.app.Hub, nil)
	s.app.Engine.OrphanGrace = 0
	h := s.Handler()
	ctx := context.Background()

	if rec := call(t, h, "POST", "/api/validate", map[string]any{"yaml": routeFlow}); rec.Code != 200 {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/api/flows/routes", map[string]any{"yaml": routeFlow, "note": "v1"}); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "GET", "/api/flows/routes", nil); rec.Code != 200 {
		t.Fatalf("get flow: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/api/flows/nope/runs", nil); rec.Code != 400 {
		t.Fatalf("start unknown flow: %d", rec.Code)
	}
	rec := call(t, h, "POST", "/api/flows/routes/runs", nil)
	if rec.Code != 201 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var run store.Run
	json.Unmarshal(rec.Body.Bytes(), &run)
	s.app.Engine.Tick(ctx)

	rec = call(t, h, "GET", "/api/runs/"+run.ID, nil)
	var got struct {
		Run    store.Run      `json:"run"`
		Visits []*store.Visit `json:"visits"`
		Graph  any            `json:"graph"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Run.Status != store.RunRunning || len(got.Visits) != 1 || got.Graph == nil {
		t.Fatalf("get run: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "GET", "/api/runs/r-missing", nil); rec.Code != 404 {
		t.Fatalf("missing run: %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/api/runs/"+run.ID+"/resume", nil); rec.Code != 409 {
		t.Fatalf("resume a running run: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/api/runs/"+run.ID+"/cancel", nil); rec.Code != 204 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	rec = call(t, h, "POST", "/api/runs/"+run.ID+"/resume", map[string]any{"node": "approve", "note": "skip to the decision"})
	if rec.Code != 200 {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body)
	}
	s.app.Engine.Tick(ctx)
	v, err := s.app.Store.LastVisit(ctx, run.ID)
	if err != nil || v.Node != "approve" || v.Status != store.VisitWaiting {
		t.Fatalf("gate should wait: %+v %v", v, err)
	}
	if rec := call(t, h, "POST", "/api/runs/"+run.ID+"/gates/"+strconv.Itoa(v.Seq), map[string]any{"outcome": "maybe"}); rec.Code != 400 {
		t.Fatalf("bad gate outcome: %d", rec.Code)
	}
	if rec := call(t, h, "POST", "/api/runs/"+run.ID+"/gates/"+strconv.Itoa(v.Seq), map[string]any{"outcome": "ship", "note": "ok"}); rec.Code != 204 {
		t.Fatalf("decide: %d %s", rec.Code, rec.Body)
	}
	s.app.Engine.Tick(ctx)
	if r, _ := s.app.Store.GetRun(ctx, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run %s", r.Status)
	}
	rec = call(t, h, "GET", "/api/runs?flow=routes", nil)
	var runs []store.Run
	json.Unmarshal(rec.Body.Bytes(), &runs)
	if rec.Code != 200 || len(runs) != 1 {
		t.Fatalf("list runs: %d %s", rec.Code, rec.Body)
	}
}
