package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

func TestShimCompleteStreamUsageAndBudget(t *testing.T) {
	testShimCompleteStreamUsageAndBudget(t, streamStopUsage)
}

func TestShimCompleteLiteLLMUsageTrailerOnce(t *testing.T) {
	// Captured LiteLLM 1.82.6 shape: stop without usage, then an empty
	// choice with a null finish reason and final usage before [DONE].
	tail := strings.Replace(streamStopUsage, `"choices":[]`, `"choices":[{"index":0,"delta":{},"finish_reason":null}]`, 1)
	testShimCompleteStreamUsageAndBudget(t, tail)
}

func testShimCompleteStreamUsageAndBudget(t *testing.T, tail string) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["stream"] != true {
			t.Error("Complete overrode stream:true")
		}
		writeCompletionStream(w, structuredReply, tail+"data: [DONE]\n\n")
	}))
	defer server.Close()
	s := NewShim(&protocol.LLMAccess{BaseURL: server.URL, Models: []protocol.ModelInfo{{Name: "gpt-6-luna"}},
		Config: &flow.LLMConfig{Limits: &flow.Limits{Tokens: 20}, OnLimit: map[string]*flow.OnLimit{flow.LimitBudgetExceeded: {Action: flow.ActOutcome}}}}, "test-grant")
	for i := 1; i <= 2; i++ {
		out, err := s.Complete(context.Background(), map[string]any{"stream": true})
		if err != nil || messageContent(out) != structuredReply {
			t.Fatalf("got %+v, %v", out, err)
		}
		usage, ok := out["usage"].(map[string]any)
		if !ok || usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(3) || usage["total_tokens"] != float64(15) {
			t.Fatalf("wrong usage values/types: %+v", out["usage"])
		}
		in, completion, calls := s.Usage()
		if in != int64(i*12) || completion != int64(i*3) || calls != i {
			t.Fatalf("usage counted incorrectly: %d/%d/%d", in, completion, calls)
		}
		if i == 1 && s.StopInfo() != nil {
			t.Fatal("interim usage was double-counted toward budget")
		}
	}
	if st := s.StopInfo(); st == nil || st.Kind != flow.LimitBudgetExceeded || st.Action != flow.ActOutcome {
		t.Fatalf("missing budget stop: %+v", st)
	}
	if out, err := s.Complete(context.Background(), map[string]any{"stream": true}); err == nil || out != nil {
		t.Fatal("stopped shim accepted another call")
	}
	if requests.Load() != 2 {
		t.Fatalf("stopped shim reached upstream: %d", requests.Load())
	}
	if in, out, calls := s.Usage(); in != 24 || out != 6 || calls != 2 {
		t.Fatalf("stopped call changed usage: %d/%d/%d", in, out, calls)
	}
}

func TestShimCompleteDefaultNonstream(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "false"}[explicit], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["stream"] != false || body["stream_options"] != nil {
					t.Error("nonstream default changed")
				}
				ok(10)(w)
			}))
			defer server.Close()
			s := NewShim(&protocol.LLMAccess{BaseURL: server.URL, Models: []protocol.ModelInfo{{Name: "test"}}}, "grant")
			body := map[string]any{}
			if explicit {
				body["stream"] = false
			}
			out, err := s.Complete(context.Background(), body)
			if err != nil || messageContent(out) != "hi" {
				t.Fatalf("got %+v, %v", out, err)
			}
			if in, completion, calls := s.Usage(); in != 10 || completion != 10 || calls != 1 {
				t.Fatalf("usage changed: %d/%d/%d", in, completion, calls)
			}
		})
	}
}

func TestShimCompleteStreamFailureNoPartialResult(t *testing.T) {
	for _, ending := range []string{"", streamStopUsage + "data: {\"error\":\"private-error\"}\n\n"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeCompletionStream(w, structuredReply, ending) }))
		s := NewShim(&protocol.LLMAccess{BaseURL: server.URL, Models: []protocol.ModelInfo{{Name: "gpt-6.1-sol"}}}, "grant")
		out, err := s.Complete(context.Background(), map[string]any{"stream": true})
		server.Close()
		if err == nil || out != nil {
			t.Fatalf("returned partial result: %+v, %v", out, err)
		}
		if in, completion, calls := s.Usage(); in != 0 || completion != 0 || calls != 1 {
			t.Fatalf("failed stream recorded successful usage: %d/%d/%d", in, completion, calls)
		}
	}
}

func TestShimCompleteStreamCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	s := NewShim(&protocol.LLMAccess{BaseURL: server.URL, Models: []protocol.ModelInfo{{Name: "gpt-6.1-sol"}}}, "grant")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		out, err := s.Complete(ctx, map[string]any{"stream": true})
		if out != nil {
			err = errors.New("returned partial result")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "partial") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not unblock stream")
	}
}
