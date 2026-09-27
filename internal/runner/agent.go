package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// DefaultAgentTools are pi's built-in tools an agent node gets.
var DefaultAgentTools = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}

func (r *Runner) runAgent(ctx context.Context) *protocol.Result {
	b := r.b
	if b.Harness != "" && b.Harness != "pi" {
		return &protocol.Result{Error: fmt.Sprintf("harness %q is not supported yet", b.Harness)}
	}
	if b.LLM == nil || len(b.LLM.Models) == 0 {
		return &protocol.Result{Error: "agent node has no model"}
	}
	shim := NewShim(b.LLM, b.Grant)
	shimURL, err := shim.Start()
	if err != nil {
		return &protocol.Result{Error: err.Error()}
	}
	defer shim.Close()

	flowDir := filepath.Join(r.work, "flow")
	piDir := filepath.Join(flowDir, "pi")
	home := filepath.Join(flowDir, "home")
	resultPath := filepath.Join(flowDir, "result.json")
	bundlePath := filepath.Join(flowDir, "bundle.json")
	for _, d := range []string{piDir, home, filepath.Join(flowDir, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return &protocol.Result{Error: err.Error()}
		}
	}
	os.Remove(resultPath)
	if err := writePiConfig(piDir, shimURL, b.LLM.Models[0]); err != nil {
		return &protocol.Result{Error: err.Error()}
	}
	skillDirs, err := writeSkills(filepath.Join(flowDir, "skills"), b.Skills)
	if err != nil {
		return &protocol.Result{Error: err.Error()}
	}
	if err := writeJSONFile(bundlePath, map[string]any{
		"outcomes":      b.Outcomes,
		"result_schema": b.ResultSchema,
		"mcp":           b.MCP,
		"pod_url":       b.PodURL,
		"node":          b.Node,
	}); err != nil {
		return &protocol.Result{Error: err.Error()}
	}
	sysPath := filepath.Join(flowDir, "system.md")
	if err := os.WriteFile(sysPath, []byte(agentSystemPrompt(b)), 0o644); err != nil {
		return &protocol.Result{Error: err.Error()}
	}

	tools := b.Tools
	if len(tools) == 0 {
		tools = DefaultAgentTools
	}
	tools = append(append([]string{}, tools...), "flow_finish")
	for _, t := range b.MCP {
		tools = append(tools, MCPToolName(t.Server, t.Name))
	}

	baseArgs := []string{"-p", "--mode", "json", "--offline",
		"--session-dir", filepath.Join(flowDir, "sessions"),
		"--no-extensions", "-e", envOr("AI_FLOW_PI_EXT", "/opt/ai-flow/pi-ext/index.ts"),
		"--no-skills", "--no-prompt-templates", "--no-themes",
		"--tools", strings.Join(tools, ","),
		"--provider", "ai-flow", "--model", b.LLM.Models[0].Name,
		"--append-system-prompt", sysPath,
	}
	for _, d := range skillDirs {
		baseArgs = append(baseArgs, "--skill", d)
	}
	if t := b.LLM.Config.Thinking; t != "" && b.LLM.Models[0].Reasoning {
		baseArgs = append(baseArgs, "--thinking", t)
	}
	env := append(cleanEnv(),
		"PI_CODING_AGENT_DIR="+piDir,
		"PI_OFFLINE=1",
		"PI_TELEMETRY=0",
		"HOME="+home,
		"NO_COLOR=1",
		"AI_FLOW_BUNDLE="+bundlePath,
		"AI_FLOW_RESULT="+resultPath,
		"AI_FLOW_GRANT="+b.Grant,
	)

	var transcript bytes.Buffer
	stop := shim.StopInfo

	prompt := b.Prompt
	var runErr error
	for attempt := 0; attempt < 2; attempt++ {
		args := append([]string{}, baseArgs...)
		if attempt > 0 {
			args = append(args, "-c")
			prompt = fmt.Sprintf("You have not called flow_finish yet. Call flow_finish now with the outcome (one of: %s), a short summary, and the outputs.", strings.Join(b.Outcomes, ", "))
			r.progress("Asking the agent to finish")
		}
		args = append(args, prompt)
		runErr = r.runPi(ctx, args, env, &transcript, shim)
		if _, err := os.Stat(resultPath); err == nil || stop() != nil || ctx.Err() != nil {
			break
		}
	}
	r.uploadTranscript(transcript.Bytes())

	if st := stop(); st != nil {
		if st.Action == flow.ActOutcome {
			return &protocol.Result{Outcome: flow.OutcomeLimit, Summary: fmt.Sprintf("Stopped by %s: %s", st.Kind, st.Message),
				Outputs: map[string]any{"limit": st.Kind}}
		}
		return &protocol.Result{Error: fmt.Sprintf("%s: %s", st.Kind, st.Message)}
	}
	res, err := readResult(resultPath)
	if err != nil {
		if ctx.Err() != nil {
			return &protocol.Result{Error: "node timed out before calling flow_finish"}
		}
		msg := "agent finished without calling flow_finish"
		if runErr != nil {
			msg += ": " + runErr.Error()
		}
		return &protocol.Result{Error: msg}
	}
	return res
}

func (r *Runner) runPi(ctx context.Context, args, env []string, transcript *bytes.Buffer, shim *Shim) error {
	pi := envOr("AI_FLOW_PI", "pi")
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-cctx.Done():
		case <-shim.Done():
			cancel()
		}
	}()
	cmd := exec.CommandContext(cctx, pi, args...)
	cmd.Dir = r.repo
	if cmd.Dir == "" {
		cmd.Dir = filepath.Join(r.work, "flow")
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start pi: %w", err)
	}
	r.progress("Agent started")
	r.followPiEvents(stdout, transcript)
	err = cmd.Wait()
	if err != nil && ctx.Err() == nil && cctx.Err() == nil {
		slog.Warn("pi exited", "err", err, "stderr", tail(stderr.String(), 2000))
		return fmt.Errorf("pi: %v: %s", err, tail(strings.TrimSpace(stderr.String()), 500))
	}
	return nil
}

// followPiEvents copies pi's JSON event stream into the transcript and turns
// tool calls and assistant text into progress lines.
func (r *Runner) followPiEvents(rd io.Reader, transcript *bytes.Buffer) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	lastUpload := time.Now()
	for sc.Scan() {
		line := sc.Bytes()
		transcript.Write(line)
		transcript.WriteByte('\n')
		// Ship the transcript so far every 15s so the UI can follow along.
		if time.Since(lastUpload) > 15*time.Second {
			lastUpload = time.Now()
			snapshot := append([]byte(nil), transcript.Bytes()...)
			go r.uploadTranscript(snapshot)
		}
		var ev struct {
			Type     string          `json:"type"`
			ToolName string          `json:"toolName"`
			Args     json.RawMessage `json:"args"`
			Message  *struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool_execution_start":
			r.progress(describeToolCall(ev.ToolName, ev.Args))
		case "message_end":
			if ev.Message != nil && ev.Message.Role == "assistant" {
				for _, c := range ev.Message.Content {
					if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
						r.progress(firstLine(c.Text))
						break
					}
				}
			}
		}
	}
}

func describeToolCall(tool string, raw json.RawMessage) string {
	var args map[string]any
	json.Unmarshal(raw, &args)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return firstLine(v)
			}
		}
		return ""
	}
	switch tool {
	case "bash":
		return "$ " + pick("command")
	case "read", "write", "edit":
		return tool + " " + pick("path", "file_path")
	case "grep", "find", "ls":
		return tool + " " + pick("pattern", "path")
	case "flow_finish":
		return "Finishing: " + pick("outcome")
	}
	return tool
}

func agentSystemPrompt(b *protocol.Bundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n# You are one step in an automated flow\n\n")
	fmt.Fprintf(&sb, "This step is %q (attempt %d). Other steps handle the rest of the task: do only what this step asks.\n", b.Node, b.Visit)
	sb.WriteString("Nobody will answer questions. Work autonomously with the tools you have.\n\n")
	sb.WriteString("## How to finish\n\n")
	sb.WriteString("When the step is done, call the `flow_finish` tool exactly once with:\n")
	fmt.Fprintf(&sb, "- `outcome`: one of %s\n", quoteList(b.Outcomes))
	sb.WriteString("- `summary`: one or two sentences on what you did and why you chose that outcome\n")
	if len(b.Outputs) > 0 {
		sb.WriteString("- `outputs`: an object with these fields:\n")
		keys := make([]string, 0, len(b.Outputs))
		for k := range b.Outputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t, _ := json.Marshal(b.Outputs[k])
			fmt.Fprintf(&sb, "  - `%s`: %s\n", k, t)
		}
	} else {
		sb.WriteString("- `outputs`: {} (this step declares no outputs)\n")
	}
	sb.WriteString("\nIf you cannot complete the step, still call flow_finish with the outcome that fits best.\n")
	if b.Repo != nil {
		if b.Repo.Write {
			sb.WriteString("Do not run `git commit` or `git push`: your working-tree changes are committed and pushed for you after you finish.\n")
		} else {
			sb.WriteString("This step is read-only: do not modify files in the repository.\n")
		}
	}
	if strings.TrimSpace(b.Context) != "" {
		sb.WriteString("\n## Context\n\n")
		sb.WriteString(b.Context)
		sb.WriteString("\n")
	}
	return sb.String()
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = "`" + x + "`"
	}
	return strings.Join(q, ", ")
}

func writePiConfig(dir, shimURL string, m protocol.ModelInfo) error {
	ctx := m.ContextTokens
	if ctx == 0 {
		ctx = 100000
	}
	models := map[string]any{
		"providers": map[string]any{
			"ai-flow": map[string]any{
				"baseUrl": shimURL,
				"api":     "openai-completions",
				"apiKey":  "ai-flow",
				"compat": map[string]any{
					"supportsDeveloperRole":   false,
					"supportsReasoningEffort": false,
				},
				"models": []map[string]any{{
					"id":            m.Name,
					"name":          m.Name,
					"reasoning":     m.Reasoning,
					"input":         []string{"text"},
					"contextWindow": ctx,
					"maxTokens":     16384,
					"cost":          map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
				}},
			},
		},
	}
	if err := writeJSONFile(filepath.Join(dir, "models.json"), models); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "settings.json"), map[string]any{"quietStartup": true})
}

func writeSkills(root string, skills map[string]protocol.SkillDir) ([]string, error) {
	var dirs []string
	names := make([]string, 0, len(skills))
	for n := range skills {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(root, name)
		for rel, content := range skills[name].Files {
			p := filepath.Join(dir, filepath.Clean("/" + rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return nil, err
			}
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func readResult(path string) (*protocol.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var res protocol.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// MCPToolName is the pi tool name for a granted MCP tool.
func MCPToolName(server, tool string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
				return r
			}
			return '_'
		}, s)
	}
	return clean(server) + "__" + clean(tool)
}

// cleanEnv passes through only what a harness needs from the pod environment.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "LANG", "LC_ALL", "TZ", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_PATH", "GOPATH", "GOCACHE", "GOMODCACHE", "PYTHONPATH":
			out = append(out, kv)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
