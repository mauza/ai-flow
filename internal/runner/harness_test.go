package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// flow-tools is flow_finish and the granted MCP tools for harnesses without
// pi's extension API: the result file is written once, with a valid outcome,
// and MCP calls go to the control plane with the step's grant.
func TestFlowToolsServer(t *testing.T) {
	var calls []string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Write([]byte(`{"content":[{"type":"text","text":"words: 5"}],"is_error":false}`))
	}))
	defer cp.Close()
	dir := t.TempDir()
	bundle, result := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "result.json")
	writeJSONFile(bundle, map[string]any{
		"outcomes":      []string{"done", "stuck", flow.OutcomeLimit},
		"result_schema": map[string]any{"properties": map[string]any{"outputs": map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}}}},
		"mcp":           []protocol.MCPTool{{Server: "demo", Name: "word_stats", Description: "Count words"}},
		"pod_url":       cp.URL,
	})
	t.Setenv("AI_FLOW_BUNDLE", bundle)
	t.Setenv("AI_FLOW_RESULT", result)
	t.Setenv("AI_FLOW_GRANT", "g1")
	srv, err := FlowToolsServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if !slices.Equal(names, []string{"demo__word_stats", "flow_finish"}) && !slices.Equal(names, []string{"flow_finish", "demo__word_stats"}) {
		t.Fatalf("tools = %v", names)
	}

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("demo__word_stats", map[string]any{"text": "a b"}); res.IsError || res.Content[0].(*mcp.TextContent).Text != "words: 5" {
		t.Fatalf("mcp proxy: %+v", res)
	}
	if len(calls) != 1 || calls[0] != "/v1/mcp/call Bearer g1" {
		t.Fatalf("control plane calls = %v", calls)
	}
	if res := call("flow_finish", map[string]any{"outcome": flow.OutcomeLimit, "summary": "x", "outputs": map[string]any{}}); !res.IsError {
		t.Fatal("limit is the runner's outcome, not the agent's")
	}
	if _, err := os.Stat(result); err == nil {
		t.Fatal("a rejected finish wrote a result")
	}
	call("flow_finish", map[string]any{"outcome": "done", "summary": "ok", "outputs": `{"n": 2}`})
	call("flow_finish", map[string]any{"outcome": "stuck", "summary": "again", "outputs": map[string]any{}})
	res, err := readResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "done" || res.Outputs["n"] != float64(2) {
		t.Fatalf("result = %+v", res)
	}
}

func testSession(t *testing.T, tools []string) *agentSession {
	dir := t.TempDir()
	return &agentSession{
		b: &protocol.Bundle{
			Node: "fix", Outcomes: []string{"done"}, Tools: tools, Grant: "g",
			LLM:   &protocol.LLMAccess{Models: []protocol.ModelInfo{{Name: "gpt-x", Reasoning: true, ThinkingFormat: "reasoning_effort", ContextTokens: 400000}}, Config: &flow.LLMConfig{Thinking: "high"}},
			Agent: &protocol.AgentSetup{Instructions: "Be careful.", ProjectInstructions: "Use make test."},
		},
		dir: dir, home: filepath.Join(dir, "home"), repo: filepath.Join(dir, "repo"), shimURL: "http://127.0.0.1:1/v1",
		system: "# Step\n\nSay \"hi\"\\n", sysPath: filepath.Join(dir, "system.md"),
		resultPath: filepath.Join(dir, "result.json"), bundlePath: filepath.Join(dir, "bundle.json"),
		skills: []string{filepath.Join(dir, "skills", "small-diffs")},
	}
}

func TestOpencodeConfig(t *testing.T) {
	s := testSession(t, AgentTools(false))
	env, err := opencodeHarness{}.setup(s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "OPENCODE_CONFIG="+filepath.Join(s.dir, "opencode.json")) || !slices.Contains(env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Fatalf("env = %v", env)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(filepath.Join(s.dir, "opencode.json"))
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["permission"].(map[string]any)["edit"] != "deny" {
		t.Fatal("a read-only step may not edit")
	}
	model := cfg["provider"].(map[string]any)["ai-flow"].(map[string]any)["models"].(map[string]any)["gpt-x"].(map[string]any)
	if model["options"].(map[string]any)["reasoningEffort"] != "high" {
		t.Fatalf("model = %v", model)
	}
	agents, _ := os.ReadFile(filepath.Join(s.home, ".config", "opencode", "AGENTS.md"))
	if !strings.Contains(string(agents), "Be careful.") || !strings.Contains(string(agents), "Use make test.") {
		t.Fatalf("AGENTS.md = %q", agents)
	}
	if target, _ := os.Readlink(filepath.Join(s.home, ".config", "opencode", "skills", "small-diffs")); target != s.skills[0] {
		t.Fatalf("skill link = %q", target)
	}
	if args := (opencodeHarness{}).args(s, "go", true); !slices.Contains(args, "--continue") || args[len(args)-1] != "go" {
		t.Fatalf("args = %v", args)
	}
}

func TestCodexConfig(t *testing.T) {
	s := testSession(t, nil)
	env, err := codexHarness{}.setup(s)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(s.dir, "codex")
	if !slices.Contains(env, "CODEX_HOME="+home) {
		t.Fatalf("env = %v", env)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	cfg := string(data)
	for _, want := range []string{
		`model_provider = "ai-flow"`,
		`model_reasoning_effort = "high"`,
		`wire_api = "responses"`,
		`base_url = "http://127.0.0.1:1/v1"`,
		`developer_instructions = "# Step\n\nSay \"hi\"\\n"`,
		"[mcp_servers.flow.env]",
		`AI_FLOW_RESULT = "` + s.resultPath + `"`,
		`[projects."` + s.repo + `"]`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.toml lacks %s:\n%s", want, cfg)
		}
	}
	args := codexHarness{}.args(s, "go", true)
	if strings.Join(args[len(args)-3:], " ") != "resume --last go" {
		t.Fatalf("args = %v", args)
	}
}

func TestTomlString(t *testing.T) {
	if got := tomlString("a\"b\\c\nd\x01é"); got != `"a\"b\\c\nd\u0001é"` {
		t.Fatalf("got %s", got)
	}
}

// codex speaks only the Responses API: the shim forwards it to the same path
// (no chat-only stream_options) and counts usage from response.completed.
func TestShimResponsesAPI(t *testing.T) {
	var path string
	var body map[string]any
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":32}}}}\n\n"))
	}))
	defer up.Close()
	s := NewShim(&protocol.LLMAccess{BaseURL: up.URL, Models: []protocol.ModelInfo{{Name: "gpt-x"}}, Config: &flow.LLMConfig{}}, "g")
	url, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := http.Post(url+"/responses", "application/json", strings.NewReader(`{"model":"gpt-x","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if path != "/responses" || body["stream_options"] != nil || !strings.Contains(string(raw), "response.completed") {
		t.Fatalf("path %s body %v reply %s", path, body, raw)
	}
	if in, out, calls := s.Usage(); in != 40 || out != 2 || calls != 1 {
		t.Fatalf("usage %d/%d/%d", in, out, calls)
	}
}
