package mcpx

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/mcpdemo"
)

func TestPoolAgainstDemoServer(t *testing.T) {
	srv := httptest.NewServer(mcpdemo.Handler())
	defer srv.Close()
	p := NewPool(map[string]config.MCPServer{"demo": {URL: srv.URL}})
	ctx := context.Background()

	tools, err := p.Tools(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s: schema %v", tl.Name, tl.InputSchema)
		}
	}
	if !names["word_stats"] || !names["time_now"] {
		t.Fatalf("tools %v", names)
	}

	res, err := p.Call(ctx, "demo", "word_stats", map[string]any{"text": "the cat and the hat"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("result %+v", res)
	}
	text, _ := res.Content[0]["text"].(string)
	if !strings.Contains(text, `"words":5`) || !strings.Contains(text, `"the"`) {
		t.Errorf("content %q", text)
	}

	if _, err := p.Tools(ctx, "nope"); err == nil {
		t.Error("unknown server should fail")
	}
}
