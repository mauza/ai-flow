package broker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/protocol"
)

func TestBrokerLLMAntigravityAndDirectUsage(t *testing.T) {
	b, c := llmTestBroker(t)
	dir := t.TempDir()
	fakeAgy := filepath.Join(dir, "fake-agy")
	script := `#!/bin/sh
cat <<'EOF'
{"status":"SUCCESS","response":"{\"outcome\":\"done\",\"summary\":\"ok\",\"outputs\":{}}","usage":{"input_tokens":300,"output_tokens":50,"cache_read_tokens":200,"total_tokens":350}}
EOF
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_BROKER_AGY_TOKEN", `{"token":"dummy"}`)

	b.cfg.Env.LLM.Upstreams["antigravity"] = config.Upstream{
		Type:    config.UpstreamAntigravity,
		AuthEnv: "TEST_BROKER_AGY_TOKEN",
		Binary:  fakeAgy,
	}
	b.cfg.Catalog.Models["gemini-4-neon-high"] = &config.Model{
		Upstream:      "antigravity",
		Model:         "gemini-4-neon-high",
		ContextTokens: 400000,
	}
	c.Models = append(c.Models, "gemini-4-neon-high")

	// Streaming chat completion through the broker for an llm node.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/llm/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-4-neon-high",
		"stream":true,
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":"go"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}
	}`))
	b.llmChat(w, req, c)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream status %d ct=%q body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	content, usage, err := llm.ReadStream(context.Background(), w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"outcome":"done"`) {
		t.Fatalf("stream content = %q", content)
	}
	if usage.PromptTokens != 500 || usage.CompletionTokens != 50 {
		t.Fatalf("stream usage = %+v", usage)
	}

	// Direct usage reporting endpoint (/v1/usage) for antigravity agent pods.
	uw := httptest.NewRecorder()
	ureq := httptest.NewRequest("POST", "/v1/usage", strings.NewReader(`{"model":"gemini-4-neon-high","tokens_in":1000,"tokens_out":200,"tokens_cached":600}`))
	b.usage(uw, ureq, c)
	if uw.Code != 200 {
		t.Fatalf("usage status %d: %s", uw.Code, uw.Body.String())
	}
	var uresp protocol.UsageResponse
	if err := json.Unmarshal(uw.Body.Bytes(), &uresp); err != nil {
		t.Fatal(err)
	}
	if uresp.OverBudget {
		t.Fatalf("unexpected over_budget: %+v", uresp)
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	// Total across llmChat (500/50/200) + /v1/usage (1000/200/600) = 1500/250/800.
	if v.TokensIn != 1500 || v.TokensOut != 250 || v.TokensCached != 800 {
		t.Fatalf("visit usage = in:%d out:%d cached:%d", v.TokensIn, v.TokensOut, v.TokensCached)
	}
}
