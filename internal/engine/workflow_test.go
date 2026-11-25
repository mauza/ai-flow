package engine_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

const workflowFlow = header + `  start: build
  nodes:
    build:
      type: action
      action: run_workflow
      with: { workflow: testflight, note: "Bill pages" }
      next: { succeeded: $success, failed: $fail, timeout: $fail }
`

func TestRunWorkflowStartsAListedWorkflowOnce(t *testing.T) {
	for conclusion, want := range map[string]string{"success": store.RunSucceeded, "failure": store.RunFailed} {
		var bodies []map[string]any
		status := "queued"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			run := map[string]any{"id": 9, "status": status, "conclusion": conclusion, "created_at": time.Now().Format(time.RFC3339), "html_url": "https://x/9"}
			switch {
			case strings.HasSuffix(r.URL.Path, "/actions/workflows/ios.yml/dispatches"):
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				bodies = append(bodies, b)
				w.WriteHeader(204)
			case strings.HasSuffix(r.URL.Path, "/actions/workflows/ios.yml/runs"):
				json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{run}})
			case strings.HasSuffix(r.URL.Path, "/actions/runs/9"):
				json.NewEncoder(w).Encode(run)
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		}))
		defer srv.Close()
		h := newHarness(t)
		h.e = engine.New(h.cfg, h.st, h.l, hub.New(), github.New(srv.URL, "t"))
		h.e.PollInterval = 0
		h.cfg.Projects["sandbox"].Spec.Workflows = map[string]*config.ProjectWorkflow{"testflight": {File: "ios.yml", Note: "changelog"}}
		id := h.flow(workflowFlow)
		h.ticks(4)
		if h.run(id).Status != store.RunRunning {
			t.Fatalf("did not wait: %s %s", h.run(id).Status, h.run(id).Error)
		}
		status = "completed"
		h.ticks(3)
		if r := h.run(id); r.Status != want {
			t.Fatalf("%s: %s %q", conclusion, r.Status, r.Error)
		}
		in, _ := bodies[0]["inputs"].(map[string]any)
		if len(bodies) != 1 || bodies[0]["ref"] != "main" || in["changelog"] != "Bill pages" {
			t.Fatalf("dispatches %v", bodies)
		}
	}
}

func TestUnlistedWorkflowIsRefused(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", YAML: workflowFlow, Project: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.CreateRun(h.ctx, "f", 0, ""); err == nil || !strings.Contains(err.Error(), "lists no workflow") {
		t.Fatalf("validation: %v", err)
	}
}
