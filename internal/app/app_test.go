package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/planner"
	"github.com/mauza/ai-flow/internal/store"
)

func TestPlanTaskPersistsFinalCandidateDiagnostics(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := "Earlier.\n```yaml\napiVersion: ai-flow/v1alpha1\nkind: Flow\nspec: {start: missing, nodes: {}}\n```"
		if calls > 0 {
			reply = "Latest.\n```yaml\nspec: [\n```"
		}
		calls++
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
	}))
	defer upstream.Close()
	cfg.Catalog.Planner.Model, cfg.Catalog.Planner.MaxAttempts = "test", 2
	cfg.Catalog.Planner.Stream = false // This diagnostics mock returns JSON, not SSE.
	cfg.Catalog.Models["test"] = &config.Model{Model: "test", Upstream: "test"}
	cfg.Env.LLM.Upstreams["test"] = config.Upstream{BaseURL: upstream.URL}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &App{Cfg: cfg, Store: st, Planner: planner.New(cfg, llm.New(cfg), nil), Hub: hub.New()}
	ctx := context.Background()
	task := &store.Task{ID: "task", Project: "sandbox", Source: "manual", Title: "Test task"}
	if err := a.CreateTask(ctx, task, false); err != nil {
		t.Fatal(err)
	}
	if err := a.planTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	task, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskPlanFailed || task.Error == "" || strings.Contains(task.Error, "missing") {
		t.Fatalf("missing or stale task error: %+v", task)
	}
	fv, err := st.GetFlow(ctx, task.FlowName, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fv.YAML != "spec: [\n" {
		t.Fatalf("wrong candidate saved: %q", fv.YAML)
	}
	chat, err := st.Chat(ctx, task.FlowName)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat) != 2 || !strings.Contains(chat[1].Content, task.Error) || !strings.HasPrefix(chat[1].Content, "Latest.") {
		t.Fatalf("chat/task diagnostics differ: %+v", chat)
	}
}

func TestInterruptedPlanningState(t *testing.T) {
	a := &App{}
	task := &store.Task{ID: "task", Status: store.TaskPlanning, FlowName: "saved", Error: "old diagnostic"}
	state, busy := a.TaskState(task)
	if busy || state.Status != TaskPlanInterrupted || state.Error == "" || state.FlowName != "saved" {
		t.Fatalf("interrupted state: %+v, busy=%v", state, busy)
	}
	if task.Status != store.TaskPlanning || task.Error != "old diagnostic" {
		t.Fatal("projection mutated persisted snapshot")
	}
	a.planning.Store(task.ID, struct{}{})
	state, busy = a.TaskState(task)
	if !busy || state.Status != store.TaskPlanning {
		t.Fatal("active planner reported interrupted")
	}
	// Restart loses ownership; classification remains correct without a scan.
	restarted := &App{}
	state, busy = restarted.TaskState(task)
	if busy || state.Status != TaskPlanInterrupted {
		t.Fatal("restart did not expose interruption")
	}
	for _, status := range []string{store.TaskNew, store.TaskFlowReady, store.TaskPlanFailed, store.TaskRunning, store.TaskFailed, store.TaskSucceeded} {
		task.Status = status
		state, _ := restarted.TaskState(task)
		if state.Status != status || state.Error != task.Error {
			t.Fatalf("changed unrelated status %q", status)
		}
	}
}

func TestSaveFlowRepairsInterruptedPlanning(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &App{Cfg: cfg, Store: st, Hub: hub.New()}
	ctx := context.Background()
	if err := st.CreateTask(ctx, &store.Task{ID: "task", Project: "sandbox", Status: store.TaskPlanning, Title: "Test", FlowName: "saved"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveFlow(ctx, &store.FlowVersion{Name: "saved", Project: "sandbox", TaskID: "task", YAML: "spec: ["}); err != nil {
		t.Fatal(err)
	}
	src := "apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: {name: saved, project: sandbox}\nspec:\n  start: ask\n  nodes:\n    ask:\n      type: gate\n      prompt: Proceed?\n      outcomes: [yes]\n      next: {yes: $success}\n"
	if _, _, err := a.SaveFlow(ctx, "saved", src, "ui", "Repair"); err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskFlowReady || task.Error != "" {
		t.Fatalf("repair did not recover task: %+v", task)
	}
	// A restart can happen after saving a valid flow but before updating its
	// task. Saving identical YAML should still repair that interrupted status.
	if err := st.UpdateTask(ctx, "task", map[string]any{"status": store.TaskPlanning}); err != nil {
		t.Fatal(err)
	}
	fv, _, err := a.SaveFlow(ctx, "saved", src, "ui", "Repair status")
	if err != nil {
		t.Fatal(err)
	}
	task, err = st.GetTask(ctx, "task")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskFlowReady || fv.Version != 2 {
		t.Fatalf("identical save failed to repair status: task=%+v flow=%+v", task, fv)
	}
}
