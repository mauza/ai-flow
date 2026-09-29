package server

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/planner"
	"github.com/mauza/ai-flow/internal/store"
)

func newTestServer(t *testing.T, token string) http.Handler {
	return testServer(t, token).Handler()
}

func testServer(t *testing.T, token string) *Server {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		t.Setenv("TEST_AI_FLOW_TOKEN", token)
		cfg.Env.Server.AuthTokenEnv = "TEST_AI_FLOW_TOKEN"
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New()}
	ui := fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}}
	s, err := New(a, nil, ui)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOverviewProjectEligibility(t *testing.T) {
	s := testServer(t, "")
	s.app.Cfg.Projects["restricted"] = &config.Project{Spec: config.ProjectSpec{Allow: config.Allow{Models: []string{"not-in-catalog"}}}}
	rec := do(s.Handler(), "GET", "/api/overview", nil)
	if rec.Code != 200 {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Projects []struct {
			Name string `json:"name"`
			planner.Eligibility
		} `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Projects) != len(s.app.Cfg.Projects) {
		t.Fatal("missing projects")
	}
	for _, p := range out.Projects {
		want := planner.ProjectEligibility(s.app.Cfg, s.app.Cfg.Projects[p.Name])
		if !reflect.DeepEqual(p.Eligibility, want) {
			t.Fatalf("%s: got %+v want %+v", p.Name, p.Eligibility, want)
		}
		if p.AllowedModels == nil || p.AllowedGrants == nil {
			t.Fatal("eligibility arrays must never be null")
		}
	}
}

func TestReadinessAndLiveness(t *testing.T) {
	s := testServer(t, "sekret")
	broker := false
	s.SetBrokerReady(func() bool { return broker })
	h := s.Handler()
	check := func(want int, db, br bool) {
		t.Helper()
		rec := do(h, "GET", "/readyz", nil)
		var body struct {
			Ready  bool            `json:"ready"`
			Checks map[string]bool `json:"checks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != want || body.Ready != (db && br) || body.Checks["database"] != db || body.Checks["broker"] != br {
			t.Fatalf("readiness: %d %s", rec.Code, rec.Body)
		}
		if rec := do(h, "GET", "/healthz", nil); rec.Code != 200 || rec.Body.String() != "ok" {
			t.Fatal("liveness depends on readiness")
		}
	}
	check(503, true, false)
	broker = true
	check(200, true, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 || time.Since(start) > time.Second {
		t.Fatal("readiness did not honor canceled context")
	}
	if err := s.app.Store.Close(); err != nil {
		t.Fatal(err)
	}
	check(503, false, true)
	broker = false
	check(503, false, false)
}

type blockingValue struct {
	entered chan struct{}
	release chan struct{}
}

func (v blockingValue) Value() (driver.Value, error) {
	close(v.entered)
	<-v.release
	return "body", nil
}

func TestReadinessBoundsDatabaseWait(t *testing.T) {
	s := testServer(t, "")
	s.SetBrokerReady(func() bool { return true })
	ctx := context.Background()
	if err := s.app.Store.CreateTask(ctx, &store.Task{ID: "blocked", Title: "Test", Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	v := blockingValue{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.app.Store.UpdateTask(ctx, "blocked", map[string]any{"body": v}) }()
	defer func() {
		close(v.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-v.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("database blocker did not start")
	}
	start := time.Now()
	rec := do(s.Handler(), "GET", "/readyz", nil)
	if rec.Code != 503 || time.Since(start) > 2*time.Second {
		t.Fatalf("database wait not bounded: %d after %s", rec.Code, time.Since(start))
	}
	if rec := do(s.Handler(), "GET", "/healthz", nil); rec.Code != 200 {
		t.Fatal("liveness blocked by database")
	}
}

func TestOperationsCompleteHistory(t *testing.T) {
	s := testServer(t, "")
	ctx := context.Background()
	stamp := store.Now()
	// Put the oldest queued run and the active run's visits beyond the default run limit.
	for i := range 205 {
		status := store.RunSucceeded
		if i < 2 {
			status = store.RunQueued
		}
		if i == 2 {
			status = store.RunRunning
		}
		if i == 3 {
			status = store.RunWaiting
		}
		run := &store.Run{ID: fmt.Sprintf("r-%d", i), FlowName: "f", Project: "p", Status: status}
		if err := s.app.Store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := s.app.Store.UpdateRun(ctx, run.ID, map[string]any{"created_at": stamp - int64(205-i)*1000}); err != nil {
			t.Fatal(err)
		}
	}
	for i, item := range []struct {
		status, outcome, message string
		start, finish            int64
	}{
		{store.VisitSucceeded, "done", "", 1000, 1300},
		{store.VisitError, "", "execution failed", 2000, 2700},
		{store.VisitError, "", "node exceeded its timeout", 3000, 4000},
		{store.VisitSucceeded, "timeout", "", 4000, 5000},
		{store.VisitCanceled, "", "", 0, 5000},
		{store.VisitSucceeded, "fail", "", 6000, 6100},
		{store.VisitRunning, "", "", 7000, 0},
	} {
		visit, err := s.app.Store.AddVisit(ctx, "r-2", "work", "check")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.app.Store.UpdateVisit(ctx, visit.RunID, visit.Seq, map[string]any{"status": item.status, "outcome": item.outcome, "error": item.message, "started_at": item.start, "finished_at": item.finish}); err != nil {
			t.Fatalf("visit %d: %v", i, err)
		}
	}
	rec := do(s.Handler(), "GET", "/api/overview", nil)
	if rec.Code != 200 {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Operations operationsView `json:"operations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	o := body.Operations
	if o.Active != 1 || o.Waiting != 1 || o.Queued != 2 || o.OldestQueuedAgeMS != o.AsOf-(stamp-205000) {
		t.Fatalf("incorrect totals: %+v", o)
	}
	if len(o.Nodes) != 1 {
		t.Fatalf("nodes: %+v", o.Nodes)
	}
	n := o.Nodes[0]
	if n.Visits != 7 || n.DurationSamples != 5 || n.DurationTotalMS != 3100 || n.DurationMaxMS != 1000 {
		t.Fatalf("incorrect durations: %+v", n)
	}
	if !reflect.DeepEqual(n.Failures, map[string]int{"error": 1, "timeout": 2, "canceled": 1, "fail_outcome": 1}) {
		t.Fatalf("categories: %v", n.Failures)
	}
}

func TestOperationsUnavailableDoesNotReturnPartialTotals(t *testing.T) {
	s := testServer(t, "")
	if err := s.app.Store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := do(s.Handler(), "GET", "/api/overview", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestInterruptedPlanningAcrossTaskAndFlowViews(t *testing.T) {
	s := testServer(t, "")
	ctx := context.Background()
	for _, id := range []string{"old", "recent"} {
		if err := s.app.Store.CreateTask(ctx, &store.Task{ID: id, Title: id, Project: "sandbox", Status: store.TaskPlanning, FlowName: "saved"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.app.Store.UpdateTask(ctx, "old", map[string]any{"created_at": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.app.Store.SaveFlow(ctx, &store.FlowVersion{Name: "saved", TaskID: "old", Project: "sandbox", YAML: "spec: ["}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, path := range []string{"/api/tasks/old", "/api/flows/saved", "/api/flows/saved/versions/1"} {
		rec := do(h, "GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var body struct {
			Task *store.Task `json:"task"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Task == nil || body.Task.Status != app.TaskPlanInterrupted || body.Task.Error == "" {
			t.Fatalf("%s: %+v", path, body.Task)
		}
	}
	rec := do(h, "GET", "/api/tasks", nil)
	var tasks []taskView
	if rec.Code != 200 {
		t.Fatalf("tasks: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks: %+v", tasks)
	}
	for _, task := range tasks {
		if task.Planning || task.Status != app.TaskPlanInterrupted {
			t.Fatalf("list state: %+v", task)
		}
	}
	// Even a task older than the list endpoint's cap gets interruption state
	// on direct access. No capped startup recovery pass is involved.
	for i := range 501 {
		if err := s.app.Store.CreateTask(ctx, &store.Task{ID: fmt.Sprintf("new-%d", i), Title: "New", Status: store.TaskSucceeded}); err != nil {
			t.Fatal(err)
		}
	}
	rec = do(h, "GET", "/api/tasks/old", nil)
	var body struct {
		Task taskView `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body.Task.Status != app.TaskPlanInterrupted || body.Task.Planning {
		t.Fatalf("old task: %d %s", rec.Code, rec.Body)
	}
}

func do(h http.Handler, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNoTokenMeansOpen(t *testing.T) {
	h := newTestServer(t, "")
	if rec := do(h, "GET", "/api/tasks", nil); rec.Code != 200 {
		t.Fatalf("GET /api/tasks: %d", rec.Code)
	}
}

func TestUnsetTokenEnvFailsClosed(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env.Server.AuthTokenEnv = "TEST_AI_FLOW_TOKEN_UNSET"
	if _, err := New(&app.App{Cfg: cfg}, nil, fstest.MapFS{}); err == nil {
		t.Fatal("unset auth token env must not start an open server")
	}
}

func TestTokenLoginFlow(t *testing.T) {
	h := newTestServer(t, "sekret")
	if rec := do(h, "GET", "/api/tasks", nil); rec.Code != 401 {
		t.Fatalf("unauthenticated API: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/tasks?token=sekret", nil); rec.Code != 401 {
		t.Fatal("tokens in API query strings must not authenticate")
	}
	// the UI page is served (it shows the "needs a token" screen itself)
	if rec := do(h, "GET", "/flows/x", nil); rec.Code != 200 {
		t.Fatalf("SPA page: %d", rec.Code)
	}
	bad := do(h, "GET", "/?token=wrong", nil)
	if len(bad.Result().Cookies()) != 0 {
		t.Fatal("wrong token set a cookie")
	}
	rec := do(h, "GET", "/runs/abc?token=sekret", nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/runs/abc" {
		t.Fatalf("login redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly {
		t.Fatalf("cookie %+v", cookies)
	}
	if rec := do(h, "GET", "/api/tasks", cookies[0]); rec.Code != 200 {
		t.Fatalf("API with cookie: %d", rec.Code)
	}
}
