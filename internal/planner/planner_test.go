package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/resolve"
)

func TestRenderRoundTrip(t *testing.T) {
	src, err := os.ReadFile("../resolve/testdata/good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := flow.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	back, err := flow.Parse([]byte(out))
	if err != nil {
		t.Fatalf("rendered YAML does not parse: %v\n%s", err, out)
	}
	a, _ := json.Marshal(f)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Errorf("round trip changed the flow\nbefore: %s\nafter:  %s\nyaml:\n%s", a, b, out)
	}
}

func TestConvergeLastCandidateDiagnostics(t *testing.T) {
	invalid := "apiVersion: ai-flow/v1alpha1\nkind: Flow\nspec: {start: absent, nodes: {}}\n"
	valid := "apiVersion: ai-flow/v1alpha1\nkind: Flow\nspec:\n  start: ask\n  nodes:\n    ask:\n      type: gate\n      prompt: Proceed?\n      outcomes: [yes]\n      next: {yes: $success}\n"
	fence := func(body string) string { return "Latest explanation.\n```yaml\n" + body + "```" }
	for _, tc := range []struct {
		name    string
		replies []string
		body    string
		valid   bool
	}{
		{"missing", []string{"No flow provided."}, "", false},
		{"empty", []string{fence(" \n")}, "", false},
		{"parse", []string{fence("spec: [\n")}, "spec: [\n", false},
		{"invalid then missing", []string{fence(invalid), "No flow provided."}, "", false},
		{"invalid then parse", []string{fence(invalid), fence("spec: [\n")}, "spec: [\n", false},
		{"parse then invalid", []string{fence("spec: [\n"), fence(invalid)}, invalid, false},
		{"parse then valid", []string{fence("spec: [\n"), fence(valid)}, valid, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load("../../deploy/config")
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls >= len(tc.replies) {
					t.Error("unexpected planner call")
					w.WriteHeader(400)
					return
				}
				reply := tc.replies[calls]
				calls++
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
			}))
			defer upstream.Close()
			cfg.Catalog.Planner.Model = "test"
			cfg.Catalog.Planner.Stream = false // This diagnostics mock returns JSON, not SSE.
			cfg.Catalog.Planner.MaxAttempts = len(tc.replies)
			cfg.Catalog.Models["test"] = &config.Model{Upstream: "test", Model: "test"}
			cfg.Env.LLM.Upstreams["test"] = config.Upstream{BaseURL: upstream.URL}
			p := New(cfg, llm.New(cfg), nil)
			req := Request{FlowName: "test", Project: "sandbox", Title: "Test"}
			res, err := p.Plan(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Valid != tc.valid || res.Attempts != len(tc.replies) || calls != len(tc.replies) {
				t.Fatalf("unexpected result: %+v, calls=%d", res, calls)
			}
			body := tc.body
			if body != "" {
				if normalized, err := p.normalize(body, req); err == nil {
					body = normalized
				}
			}
			if res.YAML != body {
				t.Fatalf("candidate mismatch: %q != %q", res.YAML, body)
			}
			if body != "" {
				want := p.validate(body, req)
				if !reflect.DeepEqual(res.Issues, want) {
					t.Fatalf("stale issues: got %+v, want %+v", res.Issues, want)
				}
			} else if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, "non-empty YAML") {
				t.Fatalf("missing extraction diagnostic: %+v", res.Issues)
			}
			if !res.Valid && !resolve.HasErrors(res.Issues) {
				t.Fatal("invalid result must explain its errors")
			}
			wantExplanation, _ := splitReply(tc.replies[len(tc.replies)-1])
			if res.Explanation != wantExplanation {
				t.Fatalf("stale explanation: %q", res.Explanation)
			}
		})
	}
}

func TestConvergeRejectsNoAttempts(t *testing.T) {
	cfg := &config.Config{}
	cfg.Catalog.Planner.Model = "test"
	for _, attempts := range []int{0, -1} {
		cfg.Catalog.Planner.MaxAttempts = attempts
		if _, err := New(cfg, nil, nil).converge(context.Background(), nil, Request{}); err == nil {
			t.Fatalf("accepted %d attempts", attempts)
		}
	}
}

func TestSplitReply(t *testing.T) {
	exp, body := splitReply("Here is the flow.\n\n```yaml\nkind: Flow\n```\nDone.")
	if body != "kind: Flow\n" || exp != "Here is the flow.\n\nDone." {
		t.Errorf("got %q / %q", exp, body)
	}
}
