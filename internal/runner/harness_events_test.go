package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// eventRunner records the progress lines a harness event parser posts.
func eventRunner(t *testing.T) (*Runner, *[]string) {
	r := runtimeRunner(t)
	var got []string
	r.http = &http.Client{Transport: runtimeTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/progress" {
			var p protocol.Progress
			b, _ := io.ReadAll(req.Body)
			json.Unmarshal(b, &p)
			got = append(got, p.Text)
		}
		resp := runtimeResponse()
		if req.URL.Path == "/v1/usage" {
			resp.Body = io.NopCloser(strings.NewReader(`{}`))
		}
		return resp, nil
	})}
	return r, &got
}

func feed(r *Runner, h harness, lines ...string) {
	for _, l := range lines {
		r.lastProgress = time.Time{} // progress is throttled; see every line here
		h.event(r, []byte(l))
	}
}

func TestOpencodeEvents(t *testing.T) {
	r, got := eventRunner(t)
	feed(r, opencodeHarness{},
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"npm test"}}}}`,
		`{"type":"text","part":{"text":"Tests pass.\nDetails follow"}}`,
		`not json`,
		`{"type":"step_finish","part":{"reason":"tool-calls"}}`,
	)
	if len(*got) != 2 || (*got)[1] != "Tests pass." {
		t.Fatalf("progress %q", *got)
	}
	if r.lastStop != "tool-calls" {
		t.Errorf("lastStop %q", r.lastStop)
	}
	feed(r, opencodeHarness{}, `{"type":"error","error":{"name":"ContextOverflow"}}`)
	if r.lastStop != `error: {"name":"ContextOverflow"}` {
		t.Errorf("lastStop %q", r.lastStop)
	}
}

func TestCodexEvents(t *testing.T) {
	r, got := eventRunner(t)
	feed(r, codexHarness{},
		`{"type":"item.started","item":{"type":"command_execution","command":"/bin/bash -lc \"go test ./...\""}}`,
		`{"type":"item.started","item":{"type":"file_change","changes":[{"path":"a.go"},{"path":"b.go"}]}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Done.\nMore"}}`,
		`{"type":"turn.completed"}`,
	)
	want := []string{"$ go test ./...", "edit a.go", "Done."}
	if len(*got) != len(want) {
		t.Fatalf("progress %q", *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("progress[%d] = %q, want %q", i, (*got)[i], want[i])
		}
	}
	if r.lastStop != "stop" {
		t.Errorf("lastStop %q", r.lastStop)
	}
	feed(r, codexHarness{}, `{"type":"turn.failed","error":{"message":"context window exceeded\nmore"}}`)
	if r.lastStop != "error: context window exceeded" {
		t.Errorf("lastStop %q", r.lastStop)
	}
}

func TestAntigravityEvents(t *testing.T) {
	r, got := eventRunner(t)
	r.shim = NewShim(&protocol.LLMAccess{
		Models: []protocol.ModelInfo{{Name: "gemini-4-neon-high"}},
		Config: &flow.LLMConfig{Limits: &flow.Limits{Tokens: 5000}},
	}, "g")
	feed(r, antigravityHarness{},
		`{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"go test ./..."}}}}`,
		`{"event":"step_update","step_update":{"state":"ACTIVE","step_type":"tool","tool_name":"mcp_flow_flow_finish","tool_info":{"name":"mcp_flow_flow_finish","parameters":{"outcome":"done"}}}}`,
		`{"event":"step_update","step_update":{"state":"DONE","step_type":"agent_response","text_delta":"All tests pass.\nDetails","usage":{"input_tokens":1000,"output_tokens":200,"cache_read_tokens":500}}}`,
		`{"event":"result","result":{"status":"SUCCESS"}}`,
	)
	want := []string{"$ go test ./...", "Finishing: done", "All tests pass."}
	if len(*got) != len(want) {
		t.Fatalf("progress %q", *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("progress[%d] = %q, want %q", i, (*got)[i], want[i])
		}
	}
	if in, out, calls := r.shim.Usage(); in != 1500 || out != 200 || calls != 1 {
		t.Fatalf("shim usage = %d/%d/%d", in, out, calls)
	}
	if r.lastStop != "success" {
		t.Errorf("lastStop = %q", r.lastStop)
	}
	feed(r, antigravityHarness{}, `{"event":"result","result":{"status":"ERROR","error":"quota exceeded\nmore"}}`)
	if r.lastStop != "error: quota exceeded" {
		t.Errorf("lastStop = %q", r.lastStop)
	}
}
