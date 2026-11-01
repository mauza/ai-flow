package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/mauza/ai-flow/internal/protocol"
)

// A harness is the coding agent an agent node runs: pi, opencode or codex.
// The runner owns everything around it (the limit shim, skills, the system
// prompt, nudges, the result file); an adapter only writes the harness's own
// config, builds its command line and reads its event stream. Every harness
// talks to the model through the shim and reports through flow_finish, which
// pi gets from pi-ext and the others from `ai-flow flow-tools` over MCP.
type harness interface {
	binary() string
	// setup writes the harness's config and returns its extra environment.
	setup(s *agentSession) ([]string, error)
	// args is one invocation; resume continues the session of the last one.
	args(s *agentSession, prompt string, resume bool) []string
	// event turns one line of the harness's JSON output into progress.
	event(r *Runner, line []byte)
	// finishTool is the name the model sees for flow_finish.
	finishTool() string
}

// Harnesses are the agent harnesses the runner can start, by catalog name.
var Harnesses = map[string]harness{"pi": piHarness{}, "opencode": opencodeHarness{}, "codex": codexHarness{}}

// agentSession is what an adapter needs to configure one agent step.
type agentSession struct {
	b          *protocol.Bundle
	dir        string // the step's private directory (/work/flow)
	home       string
	repo       string // working directory: the checkout, or dir when there is none
	shimURL    string
	skills     []string // skill directories (catalog, then repository)
	system     string   // step instructions and context (agentSystemPrompt)
	sysPath    string   // the same, as a file
	resultPath string
	bundlePath string
}

// write reports whether this step may edit the repository.
func (s *agentSession) write() bool {
	tools := s.b.Tools
	if len(tools) == 0 {
		tools = DefaultAgentTools
	}
	return slices.Contains(tools, "edit") || slices.Contains(tools, "write")
}

// instructions is the harness's and the project's agent instructions, which
// every harness reads as its global AGENTS.md.
func (s *agentSession) instructions() string {
	setup := s.b.Agent
	if setup == nil {
		return ""
	}
	var parts []string
	if v := strings.TrimSpace(setup.Instructions); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(setup.ProjectInstructions); v != "" {
		parts = append(parts, "# Project instructions\n\n"+v)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func (s *agentSession) model() protocol.ModelInfo { return s.b.LLM.Models[0] }

// thinking is the step's thinking level when the model takes one as
// reasoning_effort, the only format opencode and codex can send.
func (s *agentSession) thinking() string {
	m := s.model()
	if s.b.LLM.Config == nil || !m.Reasoning || m.ThinkingFormat != "reasoning_effort" {
		return ""
	}
	return s.b.LLM.Config.Thinking
}

func (s *agentSession) limits() (ctx, out int) {
	m := s.model()
	ctx, out = m.ContextTokens, m.MaxOutputTokens
	if ctx == 0 {
		ctx = 100000
	}
	if out == 0 {
		out = 16384
	}
	return ctx, out
}

// flowToolsEnv is what `ai-flow flow-tools` reads.
func (s *agentSession) flowToolsEnv() map[string]string {
	return map[string]string{"AI_FLOW_BUNDLE": s.bundlePath, "AI_FLOW_RESULT": s.resultPath, "AI_FLOW_GRANT": s.b.Grant}
}

// linkSkills makes each skill directory visible under root by name; the first
// of a name wins, so catalog skills shadow the repository's.
func linkSkills(root string, dirs []string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for _, d := range dirs {
		dst := filepath.Join(root, filepath.Base(d))
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.Symlink(d, dst); err != nil {
			return err
		}
	}
	return nil
}

func selfBinary() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "ai-flow"
}

// ---- pi ----

type piHarness struct{}

func (piHarness) binary() string     { return envOr("AI_FLOW_PI", "pi") }
func (piHarness) finishTool() string { return "flow_finish" }

func (piHarness) setup(s *agentSession) ([]string, error) {
	piDir := filepath.Join(s.dir, "pi")
	if err := os.MkdirAll(filepath.Join(s.dir, "sessions"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return nil, err
	}
	if err := writePiConfig(piDir, s.shimURL, s.model(), s.b.Agent); err != nil {
		return nil, err
	}
	return []string{"PI_CODING_AGENT_DIR=" + piDir, "PI_OFFLINE=1", "PI_TELEMETRY=0"}, nil
}

func (piHarness) args(s *agentSession, prompt string, resume bool) []string {
	tools := s.b.Tools
	if len(tools) == 0 {
		tools = DefaultAgentTools
	}
	tools = append(append([]string{}, tools...), "flow_finish")
	for _, t := range s.b.MCP {
		tools = append(tools, MCPToolName(t.Server, t.Name))
	}
	args := []string{"-p", "--mode", "json", "--offline",
		"--session-dir", filepath.Join(s.dir, "sessions"),
		"--no-extensions", "-e", envOr("AI_FLOW_PI_EXT", "/opt/ai-flow/pi-ext/index.ts"),
		"--no-skills", "--no-prompt-templates", "--no-themes",
		"--tools", strings.Join(tools, ","),
		"--provider", "ai-flow", "--model", s.model().Name,
		"--append-system-prompt", s.sysPath,
	}
	for _, d := range s.skills {
		args = append(args, "--skill", d)
	}
	if t := s.b.LLM.Config.Thinking; t != "" && s.model().Reasoning {
		args = append(args, "--thinking", t)
	}
	if resume {
		args = append(args, "-c")
	}
	return append(args, prompt)
}

func (piHarness) event(r *Runner, line []byte) { r.piProgress(line) }

// ---- opencode ----

type opencodeHarness struct{}

func (opencodeHarness) binary() string     { return envOr("AI_FLOW_OPENCODE", "opencode") }
func (opencodeHarness) finishTool() string { return "flow_flow_finish" }

func (opencodeHarness) setup(s *agentSession) ([]string, error) {
	cfgDir := filepath.Join(s.home, ".config", "opencode")
	if err := linkSkills(filepath.Join(cfgDir, "skills"), s.skills); err != nil {
		return nil, err
	}
	instructions := []string{s.sysPath}
	if text := s.instructions(); text != "" {
		p := filepath.Join(cfgDir, "AGENTS.md")
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			return nil, err
		}
	}
	ctx, out := s.limits()
	m := s.model()
	model := map[string]any{"name": m.Name, "reasoning": m.Reasoning, "tool_call": true, "limit": map[string]any{"context": ctx, "output": out}}
	if t := s.thinking(); t != "" {
		model["options"] = map[string]any{"reasoningEffort": t}
	}
	edit := "allow"
	if !s.write() {
		edit = "deny"
	}
	cfg := map[string]any{
		"$schema":           "https://opencode.ai/config.json",
		"autoupdate":        false,
		"share":             "disabled",
		"snapshot":          false,
		"lsp":               false,
		"formatter":         false,
		"enabled_providers": []string{"ai-flow"},
		"provider": map[string]any{"ai-flow": map[string]any{
			"npm":     "@ai-sdk/openai-compatible",
			"name":    "ai-flow",
			"options": map[string]any{"baseURL": s.shimURL, "apiKey": "ai-flow"},
			"models":  map[string]any{m.Name: model},
		}},
		"model":        "ai-flow/" + m.Name,
		"small_model":  "ai-flow/" + m.Name,
		"instructions": instructions,
		"permission": map[string]any{
			"*": "allow", "edit": edit, "webfetch": "deny", "websearch": "deny", "question": "deny",
		},
		"mcp": map[string]any{"flow": map[string]any{
			"type": "local", "command": []string{selfBinary(), "flow-tools"}, "environment": s.flowToolsEnv(), "enabled": true,
		}},
	}
	if s.b.Agent != nil {
		mergeSettings(cfg, s.b.Agent.Settings)
	}
	path := filepath.Join(s.dir, "opencode.json")
	if err := writeJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return []string{
		"OPENCODE_CONFIG=" + path,
		"OPENCODE_DISABLE_PROJECT_CONFIG=1", // the repository must not swap the model or permissions
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_FREE_MODELS=1",
		"OPENCODE_DISABLE_FILEWATCHER=1",
		"XDG_CONFIG_HOME=" + filepath.Join(s.home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(s.home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(s.home, ".cache"),
		"XDG_STATE_HOME=" + filepath.Join(s.home, ".local", "state"),
	}, nil
}

func (opencodeHarness) args(s *agentSession, prompt string, resume bool) []string {
	args := []string{"run", "--standalone", "--format", "json", "--auto", "-m", "ai-flow/" + s.model().Name}
	if resume {
		args = append(args, "--continue")
	}
	return append(args, prompt)
}

func (opencodeHarness) event(r *Runner, line []byte) {
	var ev struct {
		Type string `json:"type"`
		Part struct {
			Type   string `json:"type"`
			Tool   string `json:"tool"`
			Text   string `json:"text"`
			Reason string `json:"reason"`
			State  struct {
				Input json.RawMessage `json:"input"`
			} `json:"state"`
		} `json:"part"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "tool_use":
		r.progress(describeToolCall(strings.TrimPrefix(ev.Part.Tool, "flow_"), ev.Part.State.Input))
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			r.progress(firstLine(t))
		}
	case "step_finish":
		r.setLastStop(ev.Part.Reason)
	case "error":
		r.setLastStop("error: " + firstLine(string(ev.Error)))
	}
}

// ---- codex ----

type codexHarness struct{}

func (codexHarness) binary() string     { return envOr("AI_FLOW_CODEX", "codex") }
func (codexHarness) finishTool() string { return "flow_finish" }

func (codexHarness) setup(s *agentSession) ([]string, error) {
	home := filepath.Join(s.dir, "codex")
	if err := linkSkills(filepath.Join(home, "skills"), s.skills); err != nil {
		return nil, err
	}
	if text := s.instructions(); text != "" {
		if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(text), 0o644); err != nil {
			return nil, err
		}
	}
	ctx, _ := s.limits()
	var t tomlWriter
	t.kv("model", s.model().Name)
	t.kv("model_provider", "ai-flow")
	if th := s.thinking(); th != "" {
		t.kv("model_reasoning_effort", th)
	}
	t.kv("model_context_window", ctx)
	t.kv("approval_policy", "never")
	t.kv("sandbox_mode", "danger-full-access") // the pod is the sandbox
	t.kv("check_for_update_on_startup", false)
	t.kv("web_search", "disabled")
	t.kv("developer_instructions", s.system)
	t.table("model_providers.ai-flow")
	t.kv("name", "ai-flow")
	t.kv("base_url", s.shimURL)
	t.kv("env_key", "AI_FLOW_SHIM_KEY")
	t.kv("wire_api", "responses")
	// The shim owns retries and fallbacks (the step's on_limit policy).
	t.kv("request_max_retries", 0)
	t.kv("stream_max_retries", 0)
	t.table("mcp_servers.flow")
	t.kv("command", selfBinary())
	t.kv("args", []string{"flow-tools"})
	t.kv("startup_timeout_sec", 30)
	t.kv("tool_timeout_sec", 900)
	t.table("mcp_servers.flow.env")
	env := s.flowToolsEnv()
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.kv(k, env[k])
	}
	t.table("analytics")
	t.kv("enabled", false)
	t.table(fmt.Sprintf("projects.%s", tomlString(s.repo)))
	t.kv("trust_level", "trusted")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), t.Bytes(), 0o644); err != nil {
		return nil, err
	}
	return []string{"CODEX_HOME=" + home, "AI_FLOW_SHIM_KEY=ai-flow"}, nil
}

func (codexHarness) args(s *agentSession, prompt string, resume bool) []string {
	args := []string{"exec", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-C", s.repo}
	if resume {
		args = append(args, "resume", "--last")
	}
	return append(args, prompt)
}

func (codexHarness) event(r *Runner, line []byte) {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Command   string          `json:"command"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Changes   []struct {
				Path string `json:"path"`
			} `json:"changes"`
		} `json:"item"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "item.started":
		switch ev.Item.Type {
		case "command_execution":
			r.progress("$ " + firstLine(shellBody(ev.Item.Command)))
		case "mcp_tool_call":
			r.progress(describeToolCall(ev.Item.Tool, ev.Item.Arguments))
		case "file_change":
			for _, c := range ev.Item.Changes {
				r.progress("edit " + c.Path)
				break
			}
		}
	case "item.completed":
		if ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
			r.progress(firstLine(strings.TrimSpace(ev.Item.Text)))
		}
	case "turn.completed":
		r.setLastStop("stop")
	case "turn.failed":
		r.setLastStop("error: " + firstLine(ev.Error.Message))
	case "error":
		r.setLastStop("error: " + firstLine(ev.Message))
	}
}

// shellBody drops codex's `bash -lc "..."` wrapper from a command line.
func shellBody(cmd string) string {
	for _, sep := range []string{" -lc ", " -c "} {
		if _, body, ok := strings.Cut(cmd, sep); ok && strings.HasPrefix(cmd, "/") {
			return strings.Trim(body, `"'`)
		}
	}
	return cmd
}

// tomlWriter writes the few TOML shapes codex's config needs.
type tomlWriter struct{ bytes.Buffer }

func (t *tomlWriter) table(name string) { fmt.Fprintf(t, "\n[%s]\n", name) }

func (t *tomlWriter) kv(k string, v any) {
	key := k
	if strings.ContainsAny(k, ". ") {
		key = tomlString(k)
	}
	switch x := v.(type) {
	case string:
		fmt.Fprintf(t, "%s = %s\n", key, tomlString(x))
	case []string:
		q := make([]string, len(x))
		for i, s := range x {
			q[i] = tomlString(s)
		}
		fmt.Fprintf(t, "%s = [%s]\n", key, strings.Join(q, ", "))
	default:
		fmt.Fprintf(t, "%s = %v\n", key, x)
	}
}

// tomlString is a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
