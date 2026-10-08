package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/github/githubtest"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/workspace"
)

type productEnv struct {
	h     http.Handler
	app   *app.App
	repo  *githubtest.Repo
	files *config.Config
}

// productServer serves the API with the config in the database (as in
// production) and a fake GitHub holding o/app.
func productServer(t *testing.T) *productEnv {
	t.Helper()
	files, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, stored, err := app.LoadStoredConfig(t.Context(), st, files)
	if err != nil || !stored.Seeded {
		t.Fatalf("seed: %+v %v", stored, err)
	}
	repo := githubtest.New("o", "app", map[string]string{"product/README.md": "# App\nA small app.\n", "main.go": "package main\n"})
	repo.Description = "The app"
	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	gh := github.New(srv.URL, "")
	a := &app.App{Cfg: cfg, Store: st, Hub: hub.New(), GitHub: gh, Workspaces: workspace.New(t.TempDir(), gh, store.Now)}
	s, err := New(a, nil, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	if err != nil {
		t.Fatal(err)
	}
	return &productEnv{h: s.Handler(), app: a, repo: repo, files: files}
}

func (e *productEnv) call(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, want, rec.Body)
	}
	return rec.Body.Bytes()
}

func TestConfigEditsAreLiveAndStored(t *testing.T) {
	e := productServer(t)
	var cfgView struct {
		Sections map[string]map[string]string `json:"sections"`
		Projects map[string]string            `json:"projects"`
		SeededAt int64                        `json:"seeded_at"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/config", nil, 200), &cfgView)
	model := cfgView.Sections["models"]["gpt-6-luna"]
	if model == "" || cfgView.Projects["sandbox"] == "" || cfgView.SeededAt == 0 {
		t.Fatalf("config view: %+v", cfgView)
	}

	edited := strings.Replace(model, "notes:", "notes: Edited in the UI.", 1)
	if !strings.Contains(edited, "Edited in the UI.") {
		edited = model + "notes: Edited in the UI.\n"
	}
	e.call(t, "POST", "/api/config", map[string]any{"edits": []app.ConfigEdit{{Section: "models", Name: "gpt-6-luna", YAML: edited}}}, 200)
	if got := e.app.Cfg.Current().Catalog.Models["gpt-6-luna"].Notes; !strings.HasPrefix(got, "Edited in the UI.") {
		t.Fatalf("edit not live: %q", got)
	}
	// The overview (and everything else) reads the new version.
	if !strings.Contains(string(e.call(t, "GET", "/api/overview", nil, 200)), "Edited in the UI.") {
		t.Fatal("overview does not show the edit")
	}
	// A restart reads the stored version, not the files.
	again, stored, err := app.LoadStoredConfig(t.Context(), e.app.Store, e.files)
	if err != nil || stored.Seeded || len(stored.FromFiles) != 0 || !strings.HasPrefix(again.Catalog.Models["gpt-6-luna"].Notes, "Edited in the UI.") {
		t.Fatalf("restart: %+v err=%v", stored, err)
	}

	// Invalid edits are refused and change nothing.
	e.call(t, "POST", "/api/config", map[string]any{"edits": []app.ConfigEdit{{Section: "models", Name: "broken", YAML: "upstream: nowhere\nmodel: x\n"}}}, 400)
	e.call(t, "POST", "/api/config", map[string]any{"edits": []app.ConfigEdit{{Section: "grants", Name: "repo/ai-flow-sandbox"}}}, 400) // the sandbox project uses it
	if e.app.Cfg.Current().Catalog.Models["broken"] != nil {
		t.Fatal("a refused edit leaked")
	}
}

func TestLinkRepoAndWorkOnAStoryMap(t *testing.T) {
	e := productServer(t)
	var repos []struct {
		FullName string `json:"full_name"`
		Project  string `json:"project"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/repos", nil, 200), &repos)
	if len(repos) != 1 || repos[0].FullName != "o/app" || repos[0].Project != "" {
		t.Fatalf("repos: %+v", repos)
	}
	e.call(t, "POST", "/api/repos/link", map[string]string{"full_name": "o/app"}, 201)
	p := e.app.Cfg.Current().Projects["app"]
	if p == nil || p.Spec.Repo != "repo/app" || p.Spec.Base != "main" || p.Spec.Description != "The app" {
		t.Fatalf("linked project: %+v", p)
	}
	json.Unmarshal(e.call(t, "GET", "/api/repos", nil, 200), &repos)
	if repos[0].Project != "app" {
		t.Fatal("repo not shown as linked")
	}
	e.call(t, "POST", "/api/repos/link", map[string]string{"full_name": "o/app"}, 400) // already linked

	// The product page checks out product/ and lists docs and maps.
	var product struct {
		Docs []string `json:"docs"`
		Maps []any    `json:"maps"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/projects/app/product", nil, 200), &product)
	if len(product.Docs) != 1 || product.Docs[0] != "product/README.md" || len(product.Maps) != 0 {
		t.Fatalf("product: %+v", product)
	}

	// Create a map, add tasks, and check problems are reported.
	e.call(t, "POST", "/api/projects/app/maps", map[string]string{"id": "core", "title": "Core"}, 201)
	e.call(t, "POST", "/api/projects/app/maps", map[string]string{"id": "core", "title": "Again"}, 409)
	tasks := []map[string]any{
		{"id": "sign-up", "title": "Sign up", "activity": "first-use", "release": "mvp", "order": 1, "story": "As a user I want an account so that my progress is saved.", "acceptance": []string{"email and password work"}},
		{"id": "tour", "title": "Take the tour", "activity": "first-use", "release": "mvp", "order": 2, "status": "done"},
	}
	e.call(t, "POST", "/api/projects/app/maps/core/tasks", map[string]any{"tasks": tasks}, 204)
	e.call(t, "POST", "/api/projects/app/maps/core/tasks", map[string]any{"tasks": []map[string]any{{"id": "x", "title": "x", "activity": "nowhere"}}}, 400)

	// Add a delivery metric to the map.
	var view struct {
		Map struct {
			Map   map[string]any `json:"map"`
			Tasks []any          `json:"tasks"`
		} `json:"map"`
		Dirty []string `json:"dirty"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/projects/app/maps/core", nil, 200), &view)
	if len(view.Map.Tasks) != 2 || len(view.Dirty) != 3 {
		t.Fatalf("map view: %+v", view)
	}
	m := view.Map.Map
	m["metrics"] = []map[string]any{{"id": "mvp-done", "title": "MVP done", "kind": "delivery", "measure": "done_ratio", "release": "mvp"}}
	e.call(t, "PUT", "/api/projects/app/maps/core", map[string]any{"map": m}, 204)
	var metrics []app.MetricValue
	json.Unmarshal(e.call(t, "GET", "/api/projects/app/maps/core/metrics", nil, 200), &metrics)
	if len(metrics) != 1 || metrics[0].Value == nil || *metrics[0].Value != 50 {
		t.Fatalf("metrics: %+v", metrics)
	}

	// Send the activity to a flow: only the open task is in scope.
	var task store.Task
	json.Unmarshal(e.call(t, "POST", "/api/projects/app/maps/core/send", map[string]any{"kind": "activity", "id": "first-use", "plan": false}, 201), &task)
	if task.Source != app.SourceStoryMap || task.Project != "app" || !strings.Contains(task.Body, "As a user I want an account") || strings.Contains(task.Body, "Take the tour") {
		t.Fatalf("task: %+v", task)
	}
	var withWork struct {
		Works []app.MapWork `json:"works"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/projects/app/maps/core", nil, 200), &withWork)
	if len(withWork.Works) != 1 || strings.Join(withWork.Works[0].Scope.Tasks, ",") != "sign-up" || withWork.Works[0].Task.ID != task.ID {
		t.Fatalf("works: %+v", withWork.Works)
	}

	// Commit pushes every local change to the base branch in one commit.
	e.call(t, "POST", "/api/projects/app/workspace/commit", map[string]string{"message": "Product: core map"}, 200)
	files := e.repo.Files()
	if !strings.Contains(files["product/user-story-maps/core/tasks/sign-up.yaml"], "title: Sign up") || files["main.go"] != "package main\n" {
		t.Fatalf("repo after commit: %v", files)
	}
	// Someone else pushes; committing again needs a pull first.
	e.repo.Push(map[string]string{"product/README.md": "# App v2\n"})
	e.call(t, "PUT", "/api/projects/app/files", map[string]string{"path": "product/vision.md", "content": "# Vision\n"}, 204)
	e.call(t, "POST", "/api/projects/app/workspace/commit", map[string]string{"message": "Vision"}, 409)
	e.call(t, "POST", "/api/projects/app/workspace/pull", nil, 200)
	e.call(t, "POST", "/api/projects/app/workspace/commit", map[string]string{"message": "Vision"}, 200)
	if f := e.repo.Files(); f["product/vision.md"] != "# Vision\n" || f["product/README.md"] != "# App v2\n" {
		t.Fatalf("after pull and commit: %v", f)
	}
	// Files outside product/ cannot be written.
	e.call(t, "PUT", "/api/projects/app/files", map[string]string{"path": "main.go", "content": "x"}, 400)
}

func TestApplyAssistantChanges(t *testing.T) {
	e := productServer(t)
	e.call(t, "POST", "/api/repos/link", map[string]string{"full_name": "o/app"}, 201)
	e.call(t, "POST", "/api/projects/app/maps", map[string]string{"id": "core", "title": "Core"}, 201)
	content := "title: Invite a friend\nactivity: first-use\nrelease: mvp\n"
	var out struct {
		Problems []string `json:"problems"`
	}
	json.Unmarshal(e.call(t, "POST", "/api/projects/app/maps/core/apply", map[string]any{"changes": []map[string]any{
		{"path": "product/user-story-maps/core/tasks/invite.yaml", "content": content},
	}}, 200), &out)
	if len(out.Problems) != 0 {
		t.Fatalf("problems: %v", out.Problems)
	}
	if b, _ := e.app.Workspaces.Read("app", "product/user-story-maps/core/tasks/invite.yaml"); string(b) != content {
		t.Fatal("change not applied")
	}
	e.call(t, "POST", "/api/projects/app/maps/core/apply", map[string]any{"changes": []map[string]any{{"path": "main.go", "content": "x"}}}, 400)
}

func TestRetroOfUnknownRun(t *testing.T) {
	e := productServer(t)
	e.call(t, "POST", "/api/runs/nope/retro", nil, 404)
	var out struct {
		Retro *app.Retro `json:"retro"`
	}
	json.Unmarshal(e.call(t, "GET", "/api/runs/nope/retro", nil, 200), &out)
	if out.Retro != nil {
		t.Fatal("no retro expected")
	}
}

func TestConfigEditsThatWouldFailActiveRunsNeedForce(t *testing.T) {
	e := productServer(t)
	e.app.Engine = engine.New(e.app.Cfg, e.app.Store, nil, e.app.Hub, nil)
	src := `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: ask, project: sandbox }
spec:
  start: ask
  nodes:
    ask:
      type: llm
      model: gpt-6-luna
      prompt: Say hi.
      outcomes: [done]
      next: { done: $success }
`
	if _, issues, err := e.app.SaveFlow(t.Context(), "ask", src, "test", ""); err != nil || len(issues) > 0 {
		t.Fatalf("save: %v %v", issues, err)
	}
	run, err := e.app.Engine.CreateRun(t.Context(), "ask", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	luna := "upstream: home\nmodel: gpt-6-luna\ncontext_tokens: 100000\nreasoning: true\n"
	// Descriptive edits never affect runs.
	e.call(t, "POST", "/api/config", map[string]any{"edits": []app.ConfigEdit{{Section: "models", Name: "gpt-6-luna", YAML: luna + "notes: just a note\n"}}}, 200)
	// A change the run executes is refused, naming the run...
	changed := strings.Replace(luna, "context_tokens: 100000", "context_tokens: 50000", 1)
	body := e.call(t, "POST", "/api/config", map[string]any{"edits": []app.ConfigEdit{{Section: "models", Name: "gpt-6-luna", YAML: changed}}}, 409)
	if !strings.Contains(string(body), run.ID) {
		t.Fatalf("409 must name the run: %s", body)
	}
	if e.app.Cfg.Current().Catalog.Models["gpt-6-luna"].ContextTokens != 100000 {
		t.Fatal("a refused edit must not apply")
	}
	// ...unless forced.
	e.call(t, "POST", "/api/config", map[string]any{"force": true, "edits": []app.ConfigEdit{{Section: "models", Name: "gpt-6-luna", YAML: changed}}}, 200)
	if e.app.Cfg.Current().Catalog.Models["gpt-6-luna"].ContextTokens != 50000 {
		t.Fatal("a forced edit must apply")
	}
}
