package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatMessages(t *testing.T) {
	sys, prompt := FormatMessages([]Message{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "Hello world"},
	})
	if !strings.Contains(sys, "Be concise.") || !strings.Contains(sys, "Do not call any tools") {
		t.Fatalf("system = %q", sys)
	}
	if prompt != "Hello world" {
		t.Fatalf("single-turn prompt = %q", prompt)
	}

	_, multi := FormatMessages([]Message{
		{Role: "system", Content: "Sys"},
		{Role: "user", Content: "First"},
		{Role: "assistant", Content: "Draft"},
		{Role: "user", Content: "Fix it"},
	})
	for _, want := range []string{"## USER", "First", "## ASSISTANT", "Draft", "Fix it"} {
		if !strings.Contains(multi, want) {
			t.Fatalf("multi-turn prompt missing %q:\n%s", want, multi)
		}
	}
}

func TestCompleteWithFakeAgy(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "captured.json")
	fakeAgy := filepath.Join(dir, "fake-agy")
	script := `#!/bin/sh
set -e
# Verify isolated HOME has 0600 token, settings denying tools, and AGENTS.md
test -f "$HOME/.gemini/antigravity-cli/antigravity-oauth-token"
test -f "$HOME/.gemini/antigravity-cli/settings.json"
test -f "$HOME/.gemini/AGENTS.md"
cp "$HOME/.gemini/antigravity-cli/settings.json" "` + dir + `/seen-settings.json"
cp "$HOME/.gemini/AGENTS.md" "` + dir + `/seen-agents.md"
printf '%s\n' "$@" > "` + capture + `"
cat <<'EOF'
{"status":"SUCCESS","response":"{\"outcome\":\"done\",\"summary\":\"ok\",\"outputs\":{}}","structured_output":{"outcome":"done","summary":"ok","outputs":{}},"usage":{"input_tokens":1200,"output_tokens":45,"thinking_tokens":12,"cache_read_tokens":800,"total_tokens":1245}}
EOF
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AGY_TOKEN", `{"auth_method":"oauth","token":"dummy"}`)

	resp, err := Complete(context.Background(), CompleteRequest{
		Binary: fakeAgy,
		Model:  "gemini-4-neon-high",
		Effort: "high",
		Messages: []Message{
			{Role: "system", Content: "Return valid JSON."},
			{Role: "user", Content: "Check status"},
		},
		JSONSchema: map[string]any{"type": "object"},
		AuthEnv:    "TEST_AGY_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, `"outcome":"done"`) {
		t.Fatalf("content = %q", resp.Content)
	}
	// PromptTokens includes CachedTokens (1200 + 800 = 2000).
	if resp.Usage.PromptTokens != 2000 || resp.Usage.CompletionTokens != 45 || resp.Usage.CachedTokens != 800 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	argsBytes, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	argsStr := string(argsBytes)
	for _, want := range []string{"-p\nCheck status", "--model\ngemini-4-neon-high", "--effort\nhigh", "--output-format\njson", "--json-schema\n"} {
		if !strings.Contains(argsStr, want) {
			t.Errorf("args missing %q:\n%s", want, argsStr)
		}
	}

	var seenSettings map[string]any
	rawSettings, _ := os.ReadFile(filepath.Join(dir, "seen-settings.json"))
	if err := json.Unmarshal(rawSettings, &seenSettings); err != nil {
		t.Fatal(err)
	}
	if seenSettings["toolPermission"] != "strict" {
		t.Fatalf("settings = %v", seenSettings)
	}
	agents, _ := os.ReadFile(filepath.Join(dir, "seen-agents.md"))
	if !strings.Contains(string(agents), "Return valid JSON.") {
		t.Fatalf("AGENTS.md = %q", agents)
	}
}

func TestCompleteErrorClassification(t *testing.T) {
	dir := t.TempDir()
	fakeAgy := filepath.Join(dir, "fake-agy-err")
	script := `#!/bin/sh
echo '{"status":"ERROR","error":"RESOURCE_EXHAUSTED: rate limit exceeded (429)"}'
exit 1
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_AGY_TOKEN", `{"token":"dummy"}`)
	_, err := Complete(context.Background(), CompleteRequest{
		Binary:   fakeAgy,
		Model:    "gemini-4-neon-high",
		Messages: []Message{{Role: "user", Content: "hi"}},
		AuthEnv:  "TEST_AGY_TOKEN",
	})
	var aerr *Error
	if !errors.As(err, &aerr) || !aerr.RateLimit {
		t.Fatalf("expected RateLimit error, got %v", err)
	}
}
