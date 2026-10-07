package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

type llmTransport func(*http.Request) (*http.Response, error)

func (f llmTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func llmTestBroker(t *testing.T) (*Broker, *grant.Claims) {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := engine.New(cfg, st, nil, hub.New(), nil)
	_, err = st.SaveFlow(context.Background(), &store.FlowVersion{Name: "f", YAML: `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: f, project: sandbox}
spec:
  start: ask
  nodes:
    ask:
      type: llm
      model: ` + cfg.Catalog.Planner.Model + `
      prompt: hello
      outcomes: [done]
      next: {done: $success}
`})
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.CreateRun(context.Background(), "f", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := st.AddVisit(context.Background(), r.ID, "ask", "llm")
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{cfg: cfg, store: st, engine: e, http: &http.Client{Transport: llmTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upstream request")
		return nil, nil
	})}}
	return b, &grant.Claims{Run: r.ID, Seq: v.Seq, Node: "ask", Models: []string{cfg.Catalog.Planner.Model}}
}

func TestBudgetLookupFailureReturnsRetriable503(t *testing.T) {
	for _, failure := range []string{"store", "run", "visit", "snapshot"} {
		t.Run(failure, func(t *testing.T) {
			b, c := llmTestBroker(t)
			switch failure {
			case "store":
				if err := b.store.Close(); err != nil {
					t.Fatal(err)
				}
			case "run":
				c.Run = "missing"
			case "visit":
				c.Seq = 999
			case "snapshot":
				if err := b.store.UpdateRun(context.Background(), c.Run, map[string]any{"snapshot": `{"version":999}`}); err != nil {
					t.Fatal(err)
				}
			}
			w := httptest.NewRecorder()
			b.llmChat(w, httptest.NewRequest("POST", "/llm/v1/chat/completions", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)), c)
			if w.Code != 503 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "budget_unavailable") {
				t.Fatalf("response %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMonthlyBudgetUsesAllProjectRuns(t *testing.T) {
	b, c := llmTestBroker(t)
	ctx := context.Background()
	// Pin a monthly budget before this run's snapshot is adopted anew.
	b.cfg.Projects["sandbox"].Spec.Budget = &config.ProjBudget{USDPerMonth: 10}
	if err := b.store.UpdateRun(ctx, c.Run, map[string]any{"snapshot": ""}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	charged := &store.Run{ID: "charged", FlowName: "f", Project: "sandbox", Status: store.RunSucceeded, CostUSD: 10}
	if err := b.store.CreateRun(ctx, charged); err != nil {
		t.Fatal(err)
	}
	if err := b.store.UpdateRun(ctx, charged.ID, map[string]any{"created_at": start.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// Newer runs from another project used to hide the charge behind LIMIT 2000.
	for i := 0; i < 2001; i++ {
		r := &store.Run{ID: "noise-" + time.Unix(int64(i), 0).Format(time.RFC3339), FlowName: "other", Project: "other", Status: store.RunSucceeded}
		if err := b.store.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.overBudget(ctx, c)
	if err != nil || !strings.Contains(msg, "monthly cap") {
		t.Fatalf("msg=%q err=%v", msg, err)
	}
	w := httptest.NewRecorder()
	b.llmChat(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)), c)
	if w.Code != 429 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestUsageRecordedAfterClientCancellation(t *testing.T) {
	b, c := llmTestBroker(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.http.Transport = llmTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":12,"completion_tokens":3}}`))}, nil
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)).WithContext(ctx)
	b.llmChat(w, req, c)
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.TokensIn != 12 || v.TokensOut != 3 || v.LLMCalls != 1 {
		t.Fatalf("usage lost: %+v", v)
	}
}

func TestMissingCatalogModelDoesNotPanic(t *testing.T) {
	b, c := llmTestBroker(t)
	delete(b.cfg.Catalog.Models, c.Models[0])
	w := httptest.NewRecorder()
	b.llmChat(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)), c)
	if w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestMaxConcurrencyQueuesCallsPerModel(t *testing.T) {
	b, c := llmTestBroker(t)
	b.cfg.Catalog.Models[c.Models[0]].MaxConcurrency = 1
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, peak := 0, 0
	b.http.Transport = llmTransport(func(*http.Request) (*http.Response, error) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})
	call := func() {
		w := httptest.NewRecorder()
		b.llmChat(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)), c)
		if w.Code != 200 {
			t.Logf("call: %d %s", w.Code, w.Body)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); call() }()
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		release <- struct{}{}
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("max_concurrency 1 allowed %d calls at once", peak)
	}

	// A caller that gives up while queued leaves without taking a slot.
	b.cfg.Catalog.Models[c.Models[0]].MaxConcurrency = 1
	done := make(chan struct{})
	go func() { call(); close(done) }() // holds the only slot
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	b.llmChat(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)).WithContext(ctx), c)
	release <- struct{}{}
	<-done
	if got := len(b.modelSlots(c.Models[0], 1)); got != 0 {
		t.Fatalf("%d slots still held", got)
	}
}
