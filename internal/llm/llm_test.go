package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := &config.Config{}
	cfg.Catalog.Models = map[string]*config.Model{"planner": {Upstream: "test", Model: "chatgpt/gpt-5.5"}}
	cfg.Env.LLM.Upstreams = map[string]config.Upstream{"test": {BaseURL: server.URL + "/v1/", APIKeyEnv: "AI_FLOW_STREAM_TEST_KEY"}}
	t.Setenv("AI_FLOW_STREAM_TEST_KEY", "test-key")
	return New(cfg)
}

const (
	contentEvent = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"
	stopEvent    = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	usageEvent   = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n"
	doneEvent    = "data: [DONE]\n\n"
)

func TestChatStreamFragmentsAndUsage(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("missing auth or SSE header")
		}
		var body struct {
			Model         string    `json:"model"`
			Messages      []Message `json:"messages"`
			Temperature   float64   `json:"temperature"`
			MaxTokens     int       `json:"max_tokens"`
			Stream        bool      `json:"stream"`
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !body.Stream || !body.StreamOptions.IncludeUsage || body.Model != "chatgpt/gpt-5.5" || body.Temperature != 0.2 || body.MaxTokens != 8000 || len(body.Messages) != 1 || body.Messages[0].Content != "plan" {
			t.Errorf("unexpected request: %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		stream := "\n: heartbeat\n\nid: 1\nevent: message\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":null}}]}\n\n" +
			contentEvent + ": comment inside event\ndata: {\"choices\":\ndata: [{\"index\":0,\"delta\":{\"content\":\" 世界\\n\"}}]}\n\n" + stopEvent + usageEvent + doneEvent
		stream = strings.ReplaceAll(stream, "\n", "\r\n")
		// Flush single bytes to split JSON, UTF-8, CRLF and event boundaries.
		for i := range len(stream) {
			if _, err := io.WriteString(w, stream[i:i+1]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	text, usage, err := c.Chat(context.Background(), "planner", []Message{{Role: "user", Content: "plan"}}, Options{Stream: true, Temperature: 0.2, MaxTokens: 8000})
	if err != nil || text != "hello 世界\n" || usage != (Usage{PromptTokens: 12, CompletionTokens: 3}) {
		t.Fatalf("got text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestChatStreamTermination(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		usage        Usage
	}{
		{"done", contentEvent + doneEvent, Usage{}},
		{"stop EOF", contentEvent + stopEvent, Usage{}},
		{"stop usage EOF", contentEvent + stopEvent + usageEvent, Usage{12, 3}},
		{"stop usage done", contentEvent + stopEvent + usageEvent + doneEvent, Usage{12, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.stream) })
			text, usage, err := c.Chat(context.Background(), "planner", nil, Options{Stream: true})
			if err != nil || text != "hello" || usage != tc.usage {
				t.Fatalf("got %q, %+v, %v", text, usage, err)
			}
		})
	}
}

func TestChatStreamRejectsPartialAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name, stream, want string
		brokenTransport    bool
	}{
		{name: "empty", want: "interrupted"},
		{name: "content EOF", stream: contentEvent, want: "interrupted"},
		{name: "usage is not completion", stream: contentEvent + usageEvent, want: "interrupted"},
		{name: "bare done", stream: doneEvent, want: "no choices"},
		{name: "usage only done", stream: usageEvent + doneEvent, want: "no choices"},
		{name: "truncated JSON", stream: contentEvent + "data: {\"secret-token\":", want: "unterminated"},
		{name: "unframed done", stream: contentEvent + "data: [DONE]\n", want: "unterminated"},
		{name: "unframed stop", stream: contentEvent + strings.TrimSuffix(stopEvent, "\n"), want: "unterminated"},
		{name: "error frame", stream: contentEvent + "data: {\"error\":{\"message\":\"secret-token\"}}\n\n" + doneEvent, want: "upstream error frame"},
		{name: "error event", stream: contentEvent + "event: error\ndata: secret-token\n\n", want: "upstream error event"},
		{name: "empty error event", stream: "event: error\n\n", want: "upstream error event"},
		{name: "late error", stream: contentEvent + stopEvent + "data: {\"error\":\"secret-token\"}\n\n", want: "upstream error frame"},
		{name: "malformed JSON", stream: contentEvent + "data: secret-token\n\n" + doneEvent, want: "malformed JSON"},
		{name: "malformed content", stream: "data: {\"choices\":[{\"delta\":{\"content\":123}}]}\n\n", want: "malformed JSON"},
		{name: "null JSON", stream: "data: null\n\n", want: "no choices or usage"},
		{name: "empty choices", stream: "data: {\"choices\":[]}\n\n", want: "no choices or usage"},
		{name: "nonstream response", stream: "data: {\"choices\":[{\"message\":{\"content\":\"secret-token\"}}]}\n\n" + doneEvent, want: "no delta"},
		{name: "wrong index", stream: strings.Replace(contentEvent, "\"index\":0", "\"index\":1", 1) + doneEvent, want: "single choice"},
		{name: "length limit", stream: contentEvent + strings.Replace(stopEvent, "stop", "length", 1) + doneEvent, want: "non-stop finish"},
		{name: "filtered", stream: contentEvent + strings.Replace(stopEvent, "stop", "content_filter", 1) + doneEvent, want: "non-stop finish"},
		{name: "tool call", stream: contentEvent + strings.Replace(stopEvent, "stop", "tool_calls", 1) + doneEvent, want: "non-stop finish"},
		{name: "content after stop", stream: contentEvent + stopEvent + contentEvent + doneEvent, want: "after finish"},
		{name: "transport interrupted", stream: contentEvent, want: "reading events", brokenTransport: true},
		{name: "transport interrupted after stop", stream: contentEvent + stopEvent, want: "reading events", brokenTransport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.brokenTransport {
					w.Header().Set("Content-Length", "100000")
				}
				io.WriteString(w, tc.stream)
			})
			text, usage, err := c.Chat(context.Background(), "planner", nil, Options{Stream: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("error exposes frame data")
			}
			if text != "" || usage != (Usage{}) {
				t.Fatalf("returned partial success: %q, %+v", text, usage)
			}
			if calls.Load() != 1 {
				t.Fatalf("retried stream failure: %d calls", calls.Load())
			}
		})
	}
}

func TestChatStreamContext(t *testing.T) {
	for _, afterStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("afterStop=%t", afterStop), func(t *testing.T) {
			started := make(chan struct{})
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, contentEvent)
				if afterStop {
					io.WriteString(w, stopEvent)
				}
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				text, usage, err := c.Chat(ctx, "planner", nil, Options{Stream: true})
				if text != "" || usage != (Usage{}) {
					done <- fmt.Errorf("returned partial success")
					return
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request never started")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not unblock read")
			}
		})
	}
}

func TestStreamMemoryLimits(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 256<<10) + "\"}}]}\n\n"
	var chunks []io.Reader
	for i := 0; i <= maxStreamOutputBytes/(256<<10); i++ {
		chunks = append(chunks, strings.NewReader(chunk))
	}
	chunks = append(chunks, strings.NewReader(doneEvent))
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   string
	}{
		{"line", strings.NewReader("data: " + strings.Repeat("x", maxStreamEventBytes) + "\n\n"), "token too long"},
		{"multiline event", strings.NewReader(strings.Repeat("data: x\n", maxStreamEventBytes/8+1) + "\n"), "event exceeds"},
		{"comments", strings.NewReader(strings.Repeat(": keepalive\n", maxStreamEventBytes/12+1) + "\n"), "event exceeds"},
		{"output", io.MultiReader(chunks...), "output exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, usage, err := ReadStream(context.Background(), tc.reader)
			if err == nil || !strings.Contains(err.Error(), tc.want) || text != "" || usage != (Usage{}) {
				t.Fatalf("limit not enforced: len=%d usage=%+v err=%v", len(text), usage, err)
			}
		})
	}
}

func TestChatNonstream(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		brokenTransport  bool
	}{
		{name: "success", body: `{"choices":[{"message":{"content":"local reply"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`},
		{name: "no choices", body: `{"choices":[]}`, want: "no choices"},
		{name: "bad JSON", body: `{`, want: "bad response"},
		{name: "HTTP error", body: `bad request`, status: 400, want: "HTTP 400"},
		{name: "truncated transport", body: `{"choices":[{"message":{"content":"local reply"}}]}`, want: "reading response", brokenTransport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				for _, key := range []string{"stream", "stream_options", "max_tokens"} {
					if _, ok := body[key]; ok {
						t.Errorf("unexpected %s in default request", key)
					}
				}
				if tc.brokenTransport {
					w.Header().Set("Content-Length", "100000")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				io.WriteString(w, tc.body)
			})
			text, usage, err := c.Chat(context.Background(), "planner", nil, Options{})
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || text != "" {
					t.Fatalf("got %q, %v; want %s", text, err, tc.want)
				}
			} else if err != nil || text != "local reply" || usage != (Usage{12, 3}) {
				t.Fatalf("got %q, %+v, %v", text, usage, err)
			}
		})
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "secret-token")
	})
	text, usage, err := c.Chat(context.Background(), "planner", nil, Options{Stream: true})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), "secret-token") || text != "" || usage != (Usage{}) {
		t.Fatalf("got %q, %+v, %v", text, usage, err)
	}
}
