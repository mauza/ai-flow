package llm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// LiteLLM 1.82.6 emits a stop chunk without usage, then a usage trailer
// containing an empty choice rather than choices:[], then [DONE].
const liteLLMUsageTrailer = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n"

func TestChatStreamLiteLLMUsageTrailer(t *testing.T) {
	prefix := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" + stopEvent
	for _, tc := range []struct {
		name, trailer, ending string
	}{
		{"captured null reason", liteLLMUsageTrailer, doneEvent},
		{"omitted reason", strings.Replace(liteLLMUsageTrailer, `,"finish_reason":null`, "", 1), doneEvent},
		{"repeated stop", strings.Replace(liteLLMUsageTrailer, `"finish_reason":null`, `"finish_reason":"stop"`, 1), doneEvent},
		{"clean EOF after stop and usage", liteLLMUsageTrailer, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, prefix+tc.trailer+tc.ending)
			})
			text, usage, err := c.Chat(context.Background(), "planner", nil, Options{Stream: true})
			if err != nil || text != "hello" || usage != (Usage{12, 3}) {
				t.Fatalf("got %q, %+v, %v", text, usage, err)
			}
		})
	}
}

func TestChatStreamRejectsInvalidUsageTrailers(t *testing.T) {
	for _, tc := range []struct {
		name, trailer, ending string
		brokenTransport       bool
	}{
		{name: "text", trailer: strings.Replace(liteLLMUsageTrailer, `"delta":{}`, `"delta":{"content":"appended"}`, 1), ending: doneEvent},
		{name: "tool calls", trailer: strings.Replace(liteLLMUsageTrailer, `"delta":{}`, `"delta":{"tool_calls":[{"index":0,"function":{"name":"tool"}}]}`, 1), ending: doneEvent},
		{name: "function call", trailer: strings.Replace(liteLLMUsageTrailer, `"delta":{}`, `"delta":{"function_call":{"name":"tool"}}`, 1), ending: doneEvent},
		{name: "missing usage", trailer: stopEvent, ending: doneEvent},
		{name: "null usage", trailer: strings.Replace(liteLLMUsageTrailer, `{"prompt_tokens":12,"completion_tokens":3}`, "null", 1), ending: doneEvent},
		{name: "null delta", trailer: strings.Replace(liteLLMUsageTrailer, `"delta":{}`, `"delta":null`, 1), ending: doneEvent},
		{name: "missing delta", trailer: strings.Replace(liteLLMUsageTrailer, `"delta":{},`, "", 1), ending: doneEvent},
		{name: "non-stop finish", trailer: strings.Replace(liteLLMUsageTrailer, `"finish_reason":null`, `"finish_reason":"length"`, 1), ending: doneEvent},
		{name: "tool finish", trailer: strings.Replace(liteLLMUsageTrailer, `"finish_reason":null`, `"finish_reason":"tool_calls"`, 1), ending: doneEvent},
		{name: "wrong index", trailer: strings.Replace(liteLLMUsageTrailer, `"index":0`, `"index":1`, 1), ending: doneEvent},
		{name: "error after usage", trailer: liteLLMUsageTrailer, ending: "data: {\"error\":\"private-error\"}\n\n" + doneEvent},
		{name: "text after usage", trailer: liteLLMUsageTrailer, ending: contentEvent + doneEvent},
		{name: "truncated usage event", trailer: strings.TrimSuffix(liteLLMUsageTrailer, "\n")},
		{name: "truncated done event", trailer: liteLLMUsageTrailer, ending: "data: [DONE]\n"},
		{name: "transport interruption after usage", trailer: liteLLMUsageTrailer, brokenTransport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.brokenTransport {
					w.Header().Set("Content-Length", "100000")
				}
				io.WriteString(w, contentEvent+stopEvent+tc.trailer+tc.ending)
			})
			text, usage, err := c.Chat(context.Background(), "planner", nil, Options{Stream: true})
			if err == nil || text != "" || usage != (Usage{}) {
				t.Fatalf("returned partial success: %q, %+v, %v", text, usage, err)
			}
		})
	}
}
