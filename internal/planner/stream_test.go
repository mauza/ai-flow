package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/llm"
	"sigs.k8s.io/yaml"
)

func TestConvergeStreamConfiguration(t *testing.T) {
	const reply = "A confirmation gate.\n```yaml\nspec:\n  start: ask\n  nodes:\n    ask:\n      type: gate\n      prompt: Proceed?\n      outcomes: [yes]\n      next: {yes: $success}\n```"
	for _, tc := range []struct {
		name, setting     string
		stream, truncated bool
	}{
		{name: "default"},
		{name: "explicit false", setting: "  stream: false\n"},
		{name: "enabled", setting: "  stream: true\n", stream: true},
		{name: "truncated valid YAML", setting: "  stream: true\n", stream: true, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load("../../deploy/config")
			if err != nil {
				t.Fatal(err)
			}
			var catalog config.Catalog
			if err := yaml.Unmarshal([]byte("planner:\n  model: test\n  max_attempts: 2\n"+tc.setting), &catalog); err != nil {
				t.Fatal(err)
			}
			cfg.Catalog.Planner = catalog.Planner
			if cfg.Catalog.Planner.Stream != tc.stream {
				t.Fatal("planner.stream was not decoded")
			}
			encoded, err := json.Marshal(cfg.Catalog.Planner)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"stream"`) != tc.stream {
				t.Fatalf("stream omitempty mismatch: %s", encoded)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if tc.stream {
					if request["stream"] != true {
						t.Error("planner did not request streaming")
					}
					options, ok := request["stream_options"].(map[string]any)
					if !ok || options["include_usage"] != true {
						t.Error("planner did not request final usage")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, fragment := range []string{reply[:25], reply[25:]} {
						content, _ := json.Marshal(fragment)
						fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}]}\n\n", content)
						w.(http.Flusher).Flush()
					}
					if !tc.truncated {
						io.WriteString(w, "data: [DONE]\n\n")
					}
				} else {
					if _, ok := request["stream"]; ok {
						t.Error("default planner requested streaming")
					}
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
				}
			}))
			defer server.Close()
			cfg.Catalog.Models["test"] = &config.Model{Upstream: "test", Model: "test"}
			cfg.Env.LLM.Upstreams["test"] = config.Upstream{BaseURL: server.URL}
			p := New(cfg, llm.New(cfg), nil)
			res, err := p.converge(context.Background(), []llm.Message{{Role: "user", Content: "plan"}}, Request{FlowName: "stream-test", Project: "sandbox"})
			if tc.truncated {
				if err == nil || res != nil {
					t.Fatalf("accepted partial plan: %+v, %v", res, err)
				}
			} else if err != nil || res == nil || !res.Valid || res.Explanation != "A confirmation gate." || res.Attempts != 1 {
				t.Fatalf("unexpected plan: %+v, %v", res, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("unexpected request count: %d", calls.Load())
			}
		})
	}
}
