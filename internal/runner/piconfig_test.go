package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func piProvider(t *testing.T, dir string) (compat, model map[string]any) {
	t.Helper()
	p := readJSON(t, filepath.Join(dir, "models.json"))["providers"].(map[string]any)["ai-flow"].(map[string]any)
	return p["compat"].(map[string]any), p["models"].([]any)[0].(map[string]any)
}

// The thinking level reaches the endpoint only in the format the model names;
// without one pi sends no thinking parameter at all.
func TestPiConfigThinkingFormat(t *testing.T) {
	for _, tc := range []struct {
		format       string
		effort       bool
		piFormat     any
		maxOut, want int
	}{
		{format: "", effort: false, piFormat: nil, want: 16384},
		{format: "reasoning_effort", effort: true, piFormat: nil, maxOut: 32000, want: 32000},
		{format: "qwen-chat-template", effort: false, piFormat: "qwen-chat-template", want: 16384},
	} {
		dir := t.TempDir()
		m := protocol.ModelInfo{Name: "m", Reasoning: true, ThinkingFormat: tc.format, MaxOutputTokens: tc.maxOut}
		if err := writePiConfig(dir, "http://shim", m, nil); err != nil {
			t.Fatal(err)
		}
		compat, model := piProvider(t, dir)
		if compat["supportsReasoningEffort"] != tc.effort || compat["thinkingFormat"] != tc.piFormat {
			t.Errorf("%q: compat %v", tc.format, compat)
		}
		if model["maxTokens"] != float64(tc.want) {
			t.Errorf("%q: maxTokens %v, want %d", tc.format, model["maxTokens"], tc.want)
		}
	}
}

func TestPiSettingsAndInstructions(t *testing.T) {
	dir := t.TempDir()
	setup := &protocol.AgentSetup{
		Instructions:        "Verify before finishing.\n",
		ProjectInstructions: "Run tests with `make test`.",
		Settings: map[string]any{
			"compaction": map[string]any{"keepRecentTokens": 40000},
			"retry":      map[string]any{"maxRetries": 5},
		},
	}
	if err := writePiConfig(dir, "http://shim", protocol.ModelInfo{Name: "m"}, setup); err != nil {
		t.Fatal(err)
	}
	s := readJSON(t, filepath.Join(dir, "settings.json"))
	retry := s["retry"].(map[string]any)
	// The shim owns retries; a harness setting merges in without re-enabling them.
	if retry["enabled"] != false || retry["provider"].(map[string]any)["maxRetries"] != float64(0) || retry["maxRetries"] != float64(5) {
		t.Errorf("retry %v", retry)
	}
	if s["compaction"].(map[string]any)["keepRecentTokens"] != float64(40000) || s["quietStartup"] != true {
		t.Errorf("settings %v", s)
	}
	got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Verify before finishing.\n\n# Project instructions\n\nRun tests with `make test`.\n"; string(got) != want {
		t.Errorf("AGENTS.md %q", got)
	}

	empty := t.TempDir()
	if err := writePiConfig(empty, "http://shim", protocol.ModelInfo{Name: "m"}, &protocol.AgentSetup{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("AGENTS.md written without instructions: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoSkills(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".agents/skills/balance/SKILL.md"), "x")
	writeFile(t, filepath.Join(repo, ".claude/skills/balance/SKILL.md"), "shadowed by .agents")
	writeFile(t, filepath.Join(repo, ".claude/skills/small-diffs/SKILL.md"), "the catalog's wins")
	writeFile(t, filepath.Join(repo, ".claude/skills/triage/SKILL.md"), "x")
	writeFile(t, filepath.Join(repo, ".agents/skills/notes/README.md"), "no SKILL.md")
	got := repoSkills(repo, map[string]protocol.SkillDir{"small-diffs": {}})
	want := []string{filepath.Join(repo, ".agents/skills/balance"), filepath.Join(repo, ".claude/skills/triage")}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if repoSkills("", nil) != nil || repoSkills(t.TempDir(), nil) != nil {
		t.Error("skills without a repository")
	}
}

func TestAgentToolsReadOnly(t *testing.T) {
	if got := AgentTools(false); slices.Contains(got, "edit") || slices.Contains(got, "write") || !slices.Contains(got, "bash") {
		t.Errorf("read-only tools %v", got)
	}
	if got := AgentTools(true); !slices.Equal(got, DefaultAgentTools) {
		t.Errorf("write tools %v", got)
	}
	if len(DefaultAgentTools) != 7 {
		t.Error("AgentTools(false) modified the defaults")
	}
}

// The runner hands pi the repository's skills, the bundle's tools and the
// instructions file.
func TestAgentRunPassesRepoSkillsAndInstructions(t *testing.T) {
	r := runtimeRunner(t)
	r.repo = t.TempDir()
	writeFile(t, filepath.Join(r.repo, ".agents/skills/balance/SKILL.md"), "---\nname: balance\ndescription: x\n---\n")
	argsFile := filepath.Join(t.TempDir(), "args")
	pi := filepath.Join(t.TempDir(), "fake-pi")
	writeFile(t, pi, `#!/bin/sh
printf '%s\n' "$@" > `+argsFile+`
grep -q 'Project instructions' "$PI_CODING_AGENT_DIR/AGENTS.md" || exit 7
printf '%s' '{"outcome":"done","summary":"ok","outputs":{}}' > "$AI_FLOW_RESULT"
`)
	os.Chmod(pi, 0o700)
	t.Setenv("AI_FLOW_PI", pi)
	r.b.Type = flow.TypeAgent
	r.b.Outcomes = []string{"done"}
	r.b.Tools = AgentTools(false)
	r.b.Agent = &protocol.AgentSetup{ProjectInstructions: "Use make."}
	r.b.LLM = &protocol.LLMAccess{Models: []protocol.ModelInfo{{Name: "m", Reasoning: true, ThinkingFormat: "reasoning_effort"}}, Config: &flow.LLMConfig{Thinking: "high"}}
	if res := r.runAgent(context.Background()); res.Error != "" || res.Outcome != "done" {
		t.Fatalf("agent: %+v", res)
	}
	b, _ := os.ReadFile(argsFile)
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	flag := func(name string) []string {
		var out []string
		for i, a := range args {
			if a == name && i+1 < len(args) {
				out = append(out, args[i+1])
			}
		}
		return out
	}
	if got := flag("--skill"); !slices.Contains(got, filepath.Join(r.repo, ".agents/skills/balance")) {
		t.Errorf("--skill %v", got)
	}
	if got := flag("--tools"); len(got) != 1 || strings.Contains(got[0], "edit") || !strings.Contains(got[0], "flow_finish") {
		t.Errorf("--tools %v", got)
	}
	if got := flag("--thinking"); !slices.Equal(got, []string{"high"}) {
		t.Errorf("--thinking %v", got)
	}
}

// A fallback gets the request without the primary's thinking parameters when
// its format differs, and with the reply cap lowered to its own.
func TestShimFallbackAdaptsRequest(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			fail(429, "", "x")(w)
			return
		}
		ok(1)(w)
	}))
	t.Cleanup(srv.Close)
	s := NewShim(&protocol.LLMAccess{BaseURL: srv.URL, Config: &flow.LLMConfig{OnLimit: map[string]*flow.OnLimit{
		flow.LimitRateLimited: {Action: flow.ActFallback},
	}}, Models: []protocol.ModelInfo{
		{Name: "gpt", ThinkingFormat: "reasoning_effort"},
		{Name: "qwen", ThinkingFormat: "qwen-chat-template", MaxOutputTokens: 4096},
	}}, "grant")
	req := map[string]any{"messages": []any{}, "reasoning_effort": "high", "max_completion_tokens": float64(16384)}
	if _, err := s.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0]["reasoning_effort"] != "high" || bodies[0]["max_completion_tokens"] != float64(16384) {
		t.Fatalf("primary request %v", bodies)
	}
	if _, has := bodies[1]["reasoning_effort"]; has || bodies[1]["max_completion_tokens"] != float64(4096) || bodies[1]["model"] != "qwen" {
		t.Errorf("fallback request %v", bodies[1])
	}
	if req["reasoning_effort"] != "high" {
		t.Error("adapting the fallback request changed the caller's body")
	}
}
