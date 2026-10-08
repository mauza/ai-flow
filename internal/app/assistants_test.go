package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/workspace"
)

// fakeModel answers chat completions with canned replies, in order, and
// records the prompts it was sent.
type fakeModel struct {
	mu      sync.Mutex
	replies []string
	prompts []string
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Messages []llm.Message `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []string
	for _, m := range in.Messages {
		all = append(all, m.Role+": "+m.Content)
	}
	f.prompts = append(f.prompts, strings.Join(all, "\n"))
	reply := f.replies[0]
	if len(f.replies) > 1 {
		f.replies = f.replies[1:]
	}
	json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}})
}

func (f *fakeModel) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.prompts...)
}

func assistantApp(t *testing.T, model *fakeModel) *app.App {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	cfg.Env.LLM.Upstreams = map[string]config.Upstream{"home": {BaseURL: srv.URL}}
	cfg.Catalog.Planner.Stream = false
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &app.App{Cfg: cfg, Store: st, Hub: hub.New(), LLM: llm.New(cfg),
		Workspaces: workspace.New(t.TempDir(), github.New("http://127.0.0.1:1", ""), store.Now)}
}

func TestRetroReviewsTheRunRecord(t *testing.T) {
	model := &fakeModel{replies: []string{
		"not json",
		"Here you go: " + `{"summary": "The fix loop ran three times.", "went_well": ["tests caught it"], "problems": [{"node": "fix", "evidence": "3 visits", "impact": "$1.20"}], "suggestions": [{"kind": "primitive", "target": "loops", "change": "pass test output to the fixer", "why": "it repeated the same mistake", "priority": "high"}]}`,
	}}
	a := assistantApp(t, model)
	ctx := t.Context()
	run := &store.Run{ID: "r-1", FlowName: "fix-bug", FlowVersion: 1, Project: "sandbox", Status: store.RunFailed, Error: "fix exhausted", Branch: "b", Base: "main"}
	if err := a.Store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		v, _ := a.Store.AddVisit(ctx, run.ID, "fix", "agent")
		a.Store.UpdateVisit(ctx, run.ID, v.Seq, map[string]any{"status": store.VisitError, "error": "tests still failing: TestParse", "log_tail": "FAIL TestParse"})
	}
	r, err := a.StartRetro(ctx, run.ID, "why so many loops?")
	if err != nil || r.Status != "pending" {
		t.Fatalf("start: %+v %v", r, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ = a.GetRetro(ctx, run.ID)
		if r.Status != "pending" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.Status != "done" || r.Report.Suggestions[0].Kind != "primitive" || r.Report.Problems[0].Node != "fix" {
		t.Fatalf("retro: %+v", r)
	}
	prompts := model.calls()
	if len(prompts) != 2 {
		t.Fatalf("want a retry after the non-JSON reply, got %d calls", len(prompts))
	}
	for _, want := range []string{"#3 fix (agent, visit 3)", "tests still failing: TestParse", "FAIL TestParse", "fix exhausted", "focus on: why so many loops?", "rollback_deploy"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestMapAssistantRetriesInvalidChanges(t *testing.T) {
	model := &fakeModel{replies: []string{
		`{"reply": "Added it.", "changes": [{"path": "product/user-story-maps/core/tasks/invite.yaml", "content": "title: Invite\nactivity: nowhere\n"}]}`,
		`{"reply": "Added it under First use.", "changes": [{"path": "product/user-story-maps/core/tasks/invite.yaml", "content": "title: Invite\nactivity: first-use\n"}]}`,
	}}
	a := assistantApp(t, model)
	a.Workspaces.Write("sandbox", "product/user-story-maps/core/map.yaml", []byte(
		"title: Core\njourney:\n  - id: start\n    title: Start\n    activities:\n      - id: first-use\n        title: First use\n"))

	msg, err := a.AskMap(t.Context(), "sandbox", "core", "add an invite task")
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Issues) != 0 || len(msg.Changes) != 1 || !strings.Contains(*msg.Changes[0].Content, "first-use") {
		t.Fatalf("answer: %+v", msg)
	}
	calls := model.calls()
	if len(calls) != 2 || !strings.Contains(calls[1], `unknown activity \"nowhere\"`) && !strings.Contains(calls[1], `unknown activity "nowhere"`) {
		t.Fatalf("the invalid change must be sent back: %d calls", len(calls))
	}
	if !strings.Contains(calls[0], "--- product/user-story-maps/core/map.yaml") {
		t.Fatal("the map files must be in the prompt")
	}
	// Nothing is written until the user applies it.
	if _, err := a.Workspaces.Read("sandbox", "product/user-story-maps/core/tasks/invite.yaml"); err == nil {
		t.Fatal("asking must not change files")
	}
	history, _ := a.MapChat(t.Context(), "sandbox", "core")
	if len(history) != 2 || history[1].Content != "Added it under First use." {
		t.Fatalf("history: %+v", history)
	}
}
