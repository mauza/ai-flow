package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

const structuredReply = `{"outcome":"done","summary":"Checked","outputs":{"verified":true}}`

const streamStopUsage = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\r\n\r\n"

// Include an interim usage snapshot to catch accidental per-chunk accounting.
func writeCompletionStream(w http.ResponseWriter, content, ending string) {
	w.Header().Set("Content-Type", "text/event-stream")
	var stream strings.Builder
	stream.WriteString(": heartbeat\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":1}}\r\n\r\n")
	for _, part := range []string{content[:len(content)/2], content[len(content)/2:]} {
		data, _ := json.Marshal(part)
		fmt.Fprintf(&stream, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\r\n\r\n", data)
	}
	stream.WriteString(ending)
	raw := stream.String()
	for len(raw) > 0 {
		n := min(7, len(raw))
		if _, err := io.WriteString(w, raw[:n]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		raw = raw[n:]
	}
}

func TestRunLLMStreamsStructuredResults(t *testing.T) {
	for _, tc := range []struct {
		name, model                    string
		repair, schemaFallback, budget bool
	}{
		{name: "sol", model: "gpt-6-sol"},
		{name: "luna", model: "gpt-6-luna"},
		{name: "repair", model: "gpt-6-sol", repair: true},
		{name: "schema fallback", model: "gpt-6-luna", schemaFallback: true},
		{name: "repair accumulates budget", model: "gpt-6-sol", repair: true, budget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeRunner(t)
			r.b.Node, r.b.Prompt, r.b.Grant = "verify", "Check the result", "test-grant"
			r.b.Outcomes = []string{"done", flow.OutcomeLimit}
			r.b.ResultSchema = map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []any{"outcome", "summary", "outputs"},
				"properties": map[string]any{
					"outcome": map[string]any{"type": "string", "enum": []any{"done"}},
					"summary": map[string]any{"type": "string"},
					"outputs": map[string]any{"type": "object", "required": []any{"verified"}, "additionalProperties": false,
						"properties": map[string]any{"verified": map[string]any{"type": "boolean"}}},
				},
			}
			var calls atomic.Int32
			transcripts := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/v1/progress":
					w.WriteHeader(http.StatusNoContent)
				case "/v1/transcript":
					data, _ := io.ReadAll(req.Body)
					transcripts <- data
					w.WriteHeader(http.StatusNoContent)
				case "/v1/chat/completions":
					call := calls.Add(1)
					var body map[string]any
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer test-grant" {
						t.Error("incorrect method/auth")
					}
					if body["stream"] != true || body["model"] != tc.model {
						t.Errorf("stream/model mismatch: %+v", body)
					}
					options, _ := body["stream_options"].(map[string]any)
					if options["include_usage"] != true {
						t.Error("missing final usage request")
					}
					format, _ := body["response_format"].(map[string]any)
					if tc.schemaFallback && call == 2 {
						if format != nil {
							t.Error("schema fallback retained response_format")
						}
					} else {
						schema, _ := format["json_schema"].(map[string]any)
						if format["type"] != "json_schema" || schema["strict"] != true || !reflect.DeepEqual(schema["schema"], r.b.ResultSchema) {
							t.Error("structured schema not forwarded")
						}
					}
					if tc.schemaFallback && call == 1 {
						fail(400, "invalid_request_error", "response_format is unsupported")(w)
						return
					}
					content := structuredReply
					if tc.repair && call == 1 {
						content = `{"outcome":"unknown"}`
					}
					writeCompletionStream(w, content, streamStopUsage+"data: [DONE]\r\n\r\n")
				default:
					t.Errorf("unexpected route %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			r.podURL, r.http = server.URL, server.Client()
			r.b.LLM = &protocol.LLMAccess{BaseURL: server.URL + "/v1", Models: []protocol.ModelInfo{{Name: tc.model}}}
			if tc.budget {
				r.b.LLM.Config = &flow.LLMConfig{Limits: &flow.Limits{Tokens: 20}, OnLimit: map[string]*flow.OnLimit{flow.LimitBudgetExceeded: {Action: flow.ActOutcome}}}
			}
			res := r.runLLM(context.Background())
			if tc.budget {
				if res.Error != "" || res.Outcome != flow.OutcomeLimit || res.Outputs["limit"] != flow.LimitBudgetExceeded {
					t.Fatalf("budget not enforced: %+v", res)
				}
			} else if res.Error != "" || res.Outcome != "done" || res.Summary != "Checked" || res.Outputs["verified"] != true {
				t.Fatalf("invalid structured result: %+v", res)
			}
			wantCalls := int32(1)
			if tc.repair || tc.schemaFallback {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatalf("got %d calls, want %d", calls.Load(), wantCalls)
			}
			select {
			case transcript := <-transcripts:
				if !tc.budget && !strings.Contains(string(transcript), `"completion_tokens":3`) {
					t.Fatal("final usage missing from transcript")
				}
			default:
				t.Fatal("missing transcript upload")
			}
		})
	}
}

func TestRunLLMRejectsFailedStreams(t *testing.T) {
	for _, tc := range []struct {
		name, ending, want string
		status             int
	}{
		{name: "truncated valid result", want: "interrupted"},
		{name: "late error", ending: streamStopUsage + "data: {\"error\":{\"message\":\"private-provider-error\"}}\n\n", want: "upstream error"},
		{name: "malformed after result", ending: "data: invalid-json\n\n", want: "malformed JSON"},
		{name: "server error", status: 500, want: "server unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if tc.status != 0 {
					fail(tc.status, "server_error", "server unavailable")(w)
					return
				}
				writeCompletionStream(w, structuredReply, tc.ending)
			}))
			defer server.Close()
			r := runtimeRunner(t)
			r.b.Outcomes = []string{"done"}
			r.b.LLM = &protocol.LLMAccess{BaseURL: server.URL, Models: []protocol.ModelInfo{{Name: "gpt-6-sol"}}}
			res := r.runLLM(context.Background())
			if !strings.Contains(res.Error, tc.want) || res.Outcome != "" || len(res.Outputs) != 0 {
				t.Fatalf("failure became valid output: %+v", res)
			}
			if strings.Contains(res.Error, "private-provider-error") {
				t.Fatal("error leaked raw stream data")
			}
			if calls.Load() != 1 {
				t.Fatalf("retried failed stream: %d", calls.Load())
			}
		})
	}
}
