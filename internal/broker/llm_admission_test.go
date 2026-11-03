package broker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
)

// A call that queued for a model slot checks the budget again once it has
// one: the call ahead of it may have used the budget up.
func TestQueuedCallRechecksBudget(t *testing.T) {
	b, c := llmTestBroker(t)
	b.cfg.Catalog.Models[c.Models[0]].MaxConcurrency = 1
	b.cfg.Projects["sandbox"].Spec.Budget = &config.ProjBudget{USDPerRun: 1}
	release := make(chan struct{})
	upstream := 0
	b.http.Transport = llmTransport(func(*http.Request) (*http.Response, error) {
		upstream++
		<-release
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})
	post := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.llmChat(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`"}`)), c)
		return w
	}
	first := make(chan int)
	go func() { first <- post().Code }()
	time.Sleep(50 * time.Millisecond)
	second := make(chan *httptest.ResponseRecorder)
	go func() { second <- post() }()
	time.Sleep(50 * time.Millisecond)
	// The first call's spend lands while the second waits.
	if err := b.store.UpdateRun(context.Background(), c.Run, map[string]any{"cost_usd": 5.0}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if code := <-first; code != 200 {
		t.Fatalf("first call %d", code)
	}
	w := <-second
	if w.Code != 429 || upstream != 1 {
		t.Fatalf("queued call: %d %s (upstream calls %d)", w.Code, w.Body, upstream)
	}
}

// Raising max_concurrency applies to the next calls.
func TestMaxConcurrencyEditResizesSlots(t *testing.T) {
	b, _ := llmTestBroker(t)
	if s := b.modelSlots("m", 1); cap(s) != 1 {
		t.Fatalf("cap %d", cap(s))
	}
	if s := b.modelSlots("m", 3); cap(s) != 3 {
		t.Fatalf("after edit cap %d", cap(s))
	}
}

// The Responses API (codex) is proxied to /responses as is, and its usage,
// reported in the response.completed event, is recorded.
func TestResponsesProxyRecordsUsage(t *testing.T) {
	b, c := llmTestBroker(t)
	var path, body string
	b.http.Transport = llmTransport(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		sse := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":32}}}}\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
	w := httptest.NewRecorder()
	b.llmResponses(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"`+c.Models[0]+`","stream":true,"input":"hello"}`)), c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("reply %d %s", w.Code, w.Body)
	}
	if !strings.HasSuffix(path, "/responses") || strings.Contains(body, "stream_options") {
		t.Fatalf("upstream %s %s", path, body)
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.TokensIn != 40 || v.TokensOut != 7 || v.TokensCached != 32 {
		t.Fatalf("usage %+v", v)
	}
}
