// Package antigravity runs the official Google Antigravity CLI (`agy`) for
// non-interactive completions (planner, control-plane assistants, llm nodes)
// and shares auth/configuration helpers with the runner's antigravity harness.
package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PodAuthPath is where the Kubernetes launcher mounts an antigravity upstream's
// podSecret into agent step pods.
const PodAuthPath = "/var/run/secrets/antigravity/antigravity-oauth-token"

// Message is one chat message passed to Complete.
type Message struct {
	Role    string
	Content string
}

// CompleteRequest asks `agy` for a single non-interactive completion with all
// tools denied (no filesystem, shell, web, or MCP access).
type CompleteRequest struct {
	Binary     string
	Model      string
	Effort     string
	Messages   []Message
	JSONSchema map[string]any
	AuthEnv    string
	AuthFile   string
}

// Usage reports token usage normalized to OpenAI conventions: PromptTokens
// includes CachedTokens (matching ai-flow's broker and budget accounting).
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ThinkingTokens   int64
}

// CompleteResponse is the parsed output of `agy -p --output-format json`.
type CompleteResponse struct {
	Content          string
	StructuredOutput any
	Usage            Usage
}

// Error is returned when `agy` exits non-zero or reports a non-SUCCESS status.
type Error struct {
	Status     string
	Message    string
	RateLimit  bool
	QuotaLimit bool
	AuthError  bool
}

func (e *Error) Error() string {
	if e.Status != "" {
		return fmt.Sprintf("antigravity %s: %s", e.Status, e.Message)
	}
	return "antigravity: " + e.Message
}

// DefaultBinary returns the configured `agy` executable name or path.
func DefaultBinary() string {
	if v := strings.TrimSpace(os.Getenv("AI_FLOW_AGY")); v != "" {
		return v
	}
	return "agy"
}

// LoadAuth resolves the Antigravity OAuth token JSON from (in order):
//  1. the environment variable named by authEnv (if non-empty and set),
//  2. the file at authFile (if non-empty and readable),
//  3. AI_FLOW_ANTIGRAVITY_AUTH_FILE env var,
//  4. PodAuthPath (Kubernetes secret mount in step pods),
//  5. ANTIGRAVITY_OAUTH_TOKEN env var,
//  6. $HOME/.gemini/antigravity-cli/antigravity-oauth-token (local dev).
func LoadAuth(authEnv, authFile string) ([]byte, error) {
	if authEnv != "" {
		if v := strings.TrimSpace(os.Getenv(authEnv)); v != "" {
			return []byte(v), nil
		}
	}
	if authFile != "" {
		b, err := os.ReadFile(authFile)
		if err != nil {
			return nil, fmt.Errorf("read antigravity authFile %s: %w", authFile, err)
		}
		if len(bytes.TrimSpace(b)) > 0 {
			return bytes.TrimSpace(b), nil
		}
		return nil, fmt.Errorf("antigravity authFile %s is empty", authFile)
	}
	if p := strings.TrimSpace(os.Getenv("AI_FLOW_ANTIGRAVITY_AUTH_FILE")); p != "" {
		if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return bytes.TrimSpace(b), nil
		}
	}
	if b, err := os.ReadFile(PodAuthPath); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return bytes.TrimSpace(b), nil
	}
	if v := strings.TrimSpace(os.Getenv("ANTIGRAVITY_OAUTH_TOKEN")); v != "" {
		return []byte(v), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		p := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return bytes.TrimSpace(b), nil
		}
	}
	return nil, errors.New("antigravity oauth token not found (set authEnv/authFile, ANTIGRAVITY_OAUTH_TOKEN, or sign in with `agy`)")
}

// WriteAuth writes the OAuth token into homeDir/.gemini/antigravity-cli/antigravity-oauth-token
// with 0700/0600 permissions so `agy` can read and refresh it in an isolated HOME.
func WriteAuth(homeDir, authEnv, authFile string) error {
	tok, err := LoadAuth(authEnv, authFile)
	if err != nil {
		return err
	}
	cliDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cliDir, "antigravity-oauth-token"), tok, 0o600)
}

// Complete runs one tool-less `agy -p --output-format json` completion in an
// isolated temporary HOME and workspace directory.
func Complete(ctx context.Context, req CompleteRequest) (*CompleteResponse, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("antigravity: model is required")
	}
	bin := req.Binary
	if bin == "" {
		bin = DefaultBinary()
	}
	tmp, err := os.MkdirTemp("", "ai-flow-agy-*")
	if err != nil {
		return nil, fmt.Errorf("antigravity temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	home := filepath.Join(tmp, "home")
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	if err := WriteAuth(home, req.AuthEnv, req.AuthFile); err != nil {
		return nil, err
	}

	// Deny every tool so control-plane and llm-node completions are pure model replies.
	settings := map[string]any{
		"toolPermission": "strict",
		"permissions": map[string]any{
			"deny": []string{
				"read_file(*)",
				"write_file(*)",
				"command(*)",
				"read_url(*)",
				"execute_url(*)",
				"download_url(*)",
				"mcp(*)",
			},
		},
		"trustedWorkspaces":  []string{work},
		"showTips":           false,
		"showFeedbackSurvey": false,
		"notifications":      false,
	}
	sb, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), sb, 0o600); err != nil {
		return nil, err
	}

	system, prompt := FormatMessages(req.Messages)
	if system != "" {
		agentsPath := filepath.Join(home, ".gemini", "AGENTS.md")
		if err := os.WriteFile(agentsPath, []byte(system+"\n"), 0o644); err != nil {
			return nil, err
		}
	}

	args := []string{
		"-p", prompt,
		"--model", req.Model,
		"--output-format", "json",
		"--disable-slash-commands",
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if len(req.JSONSchema) > 0 {
		schemaPath := filepath.Join(tmp, "schema.json")
		raw, err := json.Marshal(req.JSONSchema)
		if err != nil {
			return nil, fmt.Errorf("antigravity json schema: %w", err)
		}
		if err := os.WriteFile(schemaPath, raw, 0o644); err != nil {
			return nil, err
		}
		args = append(args, "--json-schema", schemaPath)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = work
	cmd.Env = cleanExecEnv(home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var env struct {
		Status           string `json:"status"`
		Response         string `json:"response"`
		Error            string `json:"error"`
		StructuredOutput any    `json:"structured_output"`
		Usage            struct {
			InputTokens     int64 `json:"input_tokens"`
			OutputTokens    int64 `json:"output_tokens"`
			ThinkingTokens  int64 `json:"thinking_tokens"`
			CacheReadTokens int64 `json:"cache_read_tokens"`
		} `json:"usage"`
	}
	outBytes := bytes.TrimSpace(stdout.Bytes())
	// Sometimes stdout can contain leading non-JSON lines; parse the last JSON line.
	if i := bytes.LastIndexByte(outBytes, '\n'); i >= 0 && len(outBytes) > 0 && outBytes[0] != '{' {
		outBytes = bytes.TrimSpace(outBytes[i+1:])
	}
	parsed := len(outBytes) > 0 && json.Unmarshal(outBytes, &env) == nil
	if parsed && env.Status == "SUCCESS" && runErr == nil {
		content := env.Response
		if strings.TrimSpace(content) == "" && env.StructuredOutput != nil {
			if raw, err := json.Marshal(env.StructuredOutput); err == nil {
				content = string(raw)
			}
		}
		return &CompleteResponse{
			Content:          content,
			StructuredOutput: env.StructuredOutput,
			Usage: Usage{
				PromptTokens:     env.Usage.InputTokens + env.Usage.CacheReadTokens,
				CompletionTokens: env.Usage.OutputTokens,
				CachedTokens:     env.Usage.CacheReadTokens,
				ThinkingTokens:   env.Usage.ThinkingTokens,
			},
		}, nil
	}

	msg := strings.TrimSpace(env.Error)
	if msg == "" {
		msg = strings.TrimSpace(stderr.String())
	}
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	if msg == "" {
		msg = "empty response"
	}
	return nil, classifyError(env.Status, msg)
}

// FormatMessages splits system instructions (written to AGENTS.md) from the
// conversation turns passed to `agy -p`.
func FormatMessages(msgs []Message) (system, prompt string) {
	var sysParts []string
	var turns []Message
	for _, m := range msgs {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if s := strings.TrimSpace(m.Content); s != "" {
				sysParts = append(sysParts, s)
			}
		default:
			turns = append(turns, m)
		}
	}
	sysParts = append(sysParts, "Do not call any tools, read files, or run commands. Answer directly in your response.")
	system = strings.Join(sysParts, "\n\n")

	if len(turns) == 0 {
		return system, ""
	}
	if len(turns) == 1 && strings.ToLower(strings.TrimSpace(turns[0].Role)) == "user" {
		return system, turns[0].Content
	}
	var sb strings.Builder
	sb.WriteString("Continue this conversation and reply only with the next assistant response.\n\n")
	for _, t := range turns {
		role := strings.ToUpper(strings.TrimSpace(t.Role))
		if role == "" {
			role = "USER"
		}
		fmt.Fprintf(&sb, "## %s\n\n%s\n\n", role, strings.TrimSpace(t.Content))
	}
	return system, strings.TrimSpace(sb.String())
}

func classifyError(status, msg string) *Error {
	low := strings.ToLower(msg)
	return &Error{
		Status:     status,
		Message:    truncate(msg, 600),
		RateLimit:  strings.Contains(low, "rate limit") || strings.Contains(low, "429") || strings.Contains(low, "resource_exhausted") || strings.Contains(low, "too many requests"),
		QuotaLimit: strings.Contains(low, "quota") || strings.Contains(low, "capacity"),
		AuthError:  strings.Contains(low, "authentication required") || strings.Contains(low, "unauthorized") || strings.Contains(low, "401") || strings.Contains(low, "403") || strings.Contains(low, "invalid_grant"),
	}
}

func cleanExecEnv(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "LANG", "LC_ALL", "TZ", "TERM", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY":
			out = append(out, kv)
		}
	}
	return append(out,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"NO_COLOR=1",
	)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
