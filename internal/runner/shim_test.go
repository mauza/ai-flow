package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// upstream answers each call with the next scripted response and records the model asked for.
type upstream struct {
	mu     sync.Mutex
	script []func(w http.ResponseWriter)
	models []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	u.mu.Lock()
	u.models = append(u.models, body["model"].(string))
	var f func(http.ResponseWriter)
	if len(u.script) > 0 {
		f, u.script = u.script[0], u.script[1:]
	}
	u.mu.Unlock()
	if f == nil {
		f = ok(10)
	}
	f(w)
}

func ok(tokens int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}}},
			"usage":   map[string]any{"prompt_tokens": tokens, "completion_tokens": tokens},
		})
	}
}

func fail(status int, typ, msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": typ, "message": msg}})
	}
}

func newTestShim(t *testing.T, u *upstream, cfg *flow.LLMConfig, models ...string) (*Shim, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)
	var infos []protocol.ModelInfo
	for _, m := range models {
		infos = append(infos, protocol.ModelInfo{Name: m})
	}
	s := NewShim(&protocol.LLMAccess{BaseURL: srv.URL, Models: infos, Config: cfg}, "grant")
	var slept []time.Duration
	s.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return s, &slept
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func call(s *Shim) error {
	_, err := s.Complete(context.Background(), map[string]any{"messages": []any{}})
	return err
}

func TestShimRetryThenSucceed(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){fail(429, "", "slow down"), fail(503, "", "busy")}}
	s, slept := newTestShim(t, u, &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{
		flow.LimitRateLimited: {Action: flow.ActRetry, Max: 3, Backoff: flow.Duration{Duration: time.Second}},
	}}, "a")
	if err := call(s); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 2 || (*slept)[0] != time.Second {
		t.Errorf("slept %v (Retry-After 1s expected first)", *slept)
	}
	if st := s.StopInfo(); st != nil {
		t.Errorf("unexpected stop %+v", st)
	}
}

func TestShimRetryThenFallback(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){fail(429, "", "x"), fail(429, "", "x"), fail(429, "", "x")}}
	s, _ := newTestShim(t, u, &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{
		flow.LimitRateLimited: {Action: flow.ActRetry, Max: 2, Then: flow.ActFallback},
	}}, "big", "small")
	if err := call(s); err != nil {
		t.Fatal(err)
	}
	want := []string{"big", "big", "big", "small"}
	if len(u.models) != len(want) {
		t.Fatalf("models %v", u.models)
	}
	for i := range want {
		if u.models[i] != want[i] {
			t.Fatalf("models %v, want %v", u.models, want)
		}
	}
	if s.CurrentModel() != "small" {
		t.Errorf("current %s", s.CurrentModel())
	}
}

func TestShimQuotaWaitsThenFails(t *testing.T) {
	quota := fail(429, "insufficient_quota", "You exceeded your current quota")
	u := &upstream{script: []func(http.ResponseWriter){quota, quota, quota}}
	s, slept := newTestShim(t, u, &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{
		flow.LimitQuotaExhausted: {Action: flow.ActWait, MaxWait: flow.Duration{Duration: 2 * time.Second}, Then: flow.ActFail},
	}}, "a")
	if err := call(s); err == nil {
		t.Fatal("want failure after max_wait")
	}
	if len(*slept) != 2 {
		t.Errorf("slept %v, want two 1s waits within the 2s max_wait", *slept)
	}
	st := s.StopInfo()
	if st == nil || st.Kind != flow.LimitQuotaExhausted || st.Action != flow.ActFail {
		t.Errorf("stop %+v", st)
	}
	select {
	case <-s.Done():
	default:
		t.Error("Done not closed")
	}
}

func TestShimBudgetOutcome(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){fail(429, flow.LimitBudgetExceeded, "node budget of $0.10 is used up")}}
	s, _ := newTestShim(t, u, &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{
		flow.LimitBudgetExceeded: {Action: flow.ActOutcome},
	}}, "a")
	call(s)
	st := s.StopInfo()
	if st == nil || st.Kind != flow.LimitBudgetExceeded || st.Action != flow.ActOutcome {
		t.Fatalf("stop %+v", st)
	}
	// once stopped, further calls are refused without reaching upstream
	before := len(u.models)
	call(s)
	if len(u.models) != before {
		t.Error("stopped shim still called upstream")
	}
}

func TestShimContextExceededFails(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){fail(400, "", "This model's maximum context length is 8192 tokens")}}
	s, _ := newTestShim(t, u, &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{flow.LimitContextExceeded: {Action: flow.ActFail}}}, "a")
	call(s)
	if st := s.StopInfo(); st == nil || st.Kind != flow.LimitContextExceeded {
		t.Fatalf("stop %+v", st)
	}
}

func TestShimOwnTokenLimit(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){ok(400), ok(400)}}
	s, _ := newTestShim(t, u, &flow.LLMConfig{Limits: &flow.Limits{Tokens: 1000}, OnLimit: map[string]*flow.OnLimit{
		flow.LimitBudgetExceeded: {Action: flow.ActOutcome},
	}}, "a")
	srv := httptest.NewServer(http.HandlerFunc(s.handleChat)) // exercise the harness-facing path
	defer srv.Close()
	for i := 0; i < 2; i++ {
		resp, err := http.Post(srv.URL, "application/json", jsonBody(map[string]any{"messages": []any{}}))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	st := s.StopInfo()
	if st == nil || st.Kind != flow.LimitBudgetExceeded || st.Action != flow.ActOutcome {
		t.Fatalf("after 1600 tokens with a 1000 limit: stop %+v", st)
	}
}

func TestShimNonLimitErrorPassesThrough(t *testing.T) {
	u := &upstream{script: []func(http.ResponseWriter){fail(400, "invalid_request_error", "bad tool schema")}}
	s, _ := newTestShim(t, u, nil, "a")
	if err := call(s); err == nil {
		t.Fatal("want error")
	}
	if s.StopInfo() != nil {
		t.Error("a plain 400 must not stop the node")
	}
}
