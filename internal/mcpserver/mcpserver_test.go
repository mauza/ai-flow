package mcpserver_test

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/mcpserver"
	"github.com/mauza/ai-flow/internal/server"
	"github.com/mauza/ai-flow/internal/store"
)

func setupServer(t *testing.T, allowSteer bool) (*httptest.Server, *app.App, string) {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env.Server.MCPAllowSteer = allowSteer
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New()
	eng := engine.New(cfg, st, nil, h, nil)
	a := &app.App{Cfg: cfg, Store: st, Engine: eng, Hub: h}

	// Create a flow with a human gate and start a run so it is waiting.
	src := `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: gate-demo, project: sandbox }
spec:
  description: Ask the product owner before shipping.
  start: approve
  nodes:
    approve:
      type: gate
      prompt: Should we ship the new onboarding flow?
      outcomes: [approve, stop]
      next: { approve: $success, stop: $fail }
`
	if _, issues, err := a.SaveFlow(t.Context(), "gate-demo", src, "test", "init"); err != nil || len(issues) > 0 {
		t.Fatalf("save flow: %v %v", issues, err)
	}
	run, err := eng.CreateRun(t.Context(), "gate-demo", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	eng.Tick(t.Context())

	srv, err := server.New(a, nil, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, a, run.ID
}

func connectHTTP(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func toolNames(t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s returned error: %+v", name, res.Content)
	}
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return strings.Join(out, "\n")
}

func TestControlPlaneMCPReadOnlyByDefault(t *testing.T) {
	ts, _, runID := setupServer(t, false)
	sess := connectHTTP(t, ts.URL+"/mcp")
	names := toolNames(t, sess)
	for _, want := range []string{"get_overview", "get_attention", "explain_run", "get_product_summary", "get_project_digest", "get_improvement_summary", "list_run_grades", "list_improvement_proposals"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing read tool %q in %v", want, names)
		}
	}
	for _, forbidden := range []string{"decide_gate", "steer_run", "resume_run", "create_task", "grade_run", "run_improver", "decide_proposal"} {
		if slices.Contains(names, forbidden) {
			t.Errorf("steer tool %q must not be exposed in read-only mode", forbidden)
		}
	}
	imp := callText(t, sess, "get_improvement_summary", nil)
	if !strings.Contains(imp, "efficiency") || !strings.Contains(imp, "instruction_adherence") {
		t.Errorf("get_improvement_summary missing starter rubrics:\n%s", imp)
	}

	att := callText(t, sess, "get_attention", nil)
	if !strings.Contains(att, runID) || !strings.Contains(att, "Should we ship the new onboarding flow?") {
		t.Errorf("get_attention missing waiting gate:\n%s", att)
	}
	exp := callText(t, sess, "explain_run", map[string]any{"run_id": runID})
	if !strings.Contains(exp, "gate-demo") || !strings.Contains(exp, "active_gate") {
		t.Errorf("explain_run missing orchestration details:\n%s", exp)
	}
	digest := callText(t, sess, "get_project_digest", map[string]any{"project": "sandbox"})
	if !strings.Contains(digest, "# sandbox") || !strings.Contains(digest, "Needs Human Attention") {
		t.Errorf("unexpected digest:\n%s", digest)
	}
}

func TestControlPlaneMCPSteeringAndHTTPBridge(t *testing.T) {
	ts, a, runID := setupServer(t, true)

	// Exercise the HTTPBackend (used by `ai-flow mcp -url ... -allow-steer`)
	// end-to-end over an in-memory MCP transport.
	b := mcpserver.NewHTTPBackend(ts.URL, "")
	srv := mcpserver.NewServer(b, mcpserver.Options{AllowSteer: true, Actor: "local-llm"})
	t1, t2 := mcp.NewInMemoryTransports()
	go srv.Run(t.Context(), t1)
	client := mcp.NewClient(&mcp.Implementation{Name: "cli-test", Version: "1"}, nil)
	sess, err := client.Connect(t.Context(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if !slices.Contains(toolNames(t, sess), "decide_gate") {
		t.Fatal("decide_gate must be present when AllowSteer is true")
	}
	reply := callText(t, sess, "decide_gate", map[string]any{
		"run_id": runID, "seq": 1, "outcome": "approve", "note": "Looks great to ship",
	})
	if !strings.Contains(reply, "approve") {
		t.Fatalf("unexpected reply: %s", reply)
	}
	r, err := a.Store.GetRun(t.Context(), runID)
	if err != nil || r.Status != store.RunSucceeded {
		t.Fatalf("run after MCP decide_gate: %+v %v", r, err)
	}
}
