package app_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/github/githubtest"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
	"github.com/mauza/ai-flow/internal/workspace"
)

func TestHoneMapsClosesShippedWorkValidatesAndCommits(t *testing.T) {
	ctx := t.Context()
	repo := githubtest.New("acme", "demo", map[string]string{
		"README.md":         "# Demo\nCore feature set: search and alerts.",
		"product/README.md": "# Demo Product\n## Mission\nMake civic bills legible.\n## Vision\nEvery resident understands state bills in 20 minutes a week.\n",
		"product/user-story-maps/core/map.yaml": `title: Core Loop
journey:
  - id: discover
    title: Discover
    activities:
      - id: read
        title: Read bills
releases:
  - id: r1
    title: R1
    goal: Ship first loop
    status: in_progress
  - id: r2
    title: R2
    goal: Next slice
    status: planned
`,
		"product/user-story-maps/core/tasks/shipped-card.yaml": `title: Public bill page
activity: read
release: r1
order: 1
status: todo
`,
		"product/user-story-maps/ops/map.yaml": `title: Quality & Ops
journey:
  - id: reliability
    title: Reliability
    activities:
      - id: test
        title: Resilience tests
releases:
  - id: r1
    title: R1
    status: in_progress
`,
	})
	ghSrv := httptest.NewServer(repo)
	defer ghSrv.Close()
	gh := github.New(ghSrv.URL, "test-token")

	// First LLM reply tries to edit product/README.md (forbidden) and must be repaired.
	badReply := `{"summary": "bad edit", "changes": [{"path": "product/README.md", "content": "overwritten"}]}`
	goodReply := `{"summary": "Closed shipped R1 and added next R2 card", "changes": [{"path": "product/user-story-maps/core/tasks/follow-up.yaml", "content": "title: Local impact card\nactivity: read\nrelease: r2\norder: 1\nstatus: todo\ndepends_on:\n  - shipped-card\nacceptance:\n  - Shows coarse locality summary\n"}]}`
	model := &fakeModel{replies: []string{badReply, goodReply}}

	a := assistantApp(t, model)
	a.GitHub = gh
	a.Workspaces = workspace.New(t.TempDir(), gh, store.Now)
	a.Cfg.Catalog.Grants["repo/ai-flow-sandbox"].URL = "https://github.com/acme/demo.git"
	a.Cfg.Projects["sandbox"].Spec.Base = "main"
	a.Cfg.Projects["sandbox"].Spec.Schedules = []config.ProjectSchedule{{
		Name:     "hone-storymaps",
		Every:    flow.Duration{Duration: 12 * time.Hour},
		HoneMaps: &config.ScheduleHoneMaps{ActiveMaps: 2, MinReadyTasks: 2, MaxNewTasks: 3},
	}}

	// Seed a succeeded run covering core/shipped-card so deterministic sync closes it.
	if _, err := a.Workspaces.Ensure(ctx, "sandbox", workspace.Source{Repo: github.Repo{Host: "github.com", Owner: "acme", Name: "demo"}, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	sent, err := a.SendToFlow(ctx, "sandbox", "core", "task", "shipped-card", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.CreateRun(ctx, &store.Run{
		ID: "r-shipped", FlowName: "shipped-card", FlowVersion: 1, TaskID: sent.ID,
		Project: "sandbox", Status: store.RunSucceeded, PRURL: "https://github.com/acme/demo/pull/42",
		Branch: "ai-flow/shipped", Base: "main",
	}); err != nil {
		t.Fatal(err)
	}

	st, err := a.TriggerSchedule(ctx, "sandbox", "hone-storymaps", false)
	if err != nil {
		t.Fatalf("hone_maps trigger failed: %v", err)
	}
	if st.LastStatus != "started" {
		t.Fatalf("want started, got %+v", st)
	}

	// Verify model received mission/vision, repo README, and retried after bad path.
	calls := model.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls (initial + repair), got %d", len(calls))
	}
	for _, want := range []string{"Make civic bills legible", "Core feature set: search and alerts", "https://github.com/acme/demo/pull/42"} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !strings.Contains(calls[1], "product/README.md") {
		t.Errorf("repair prompt should mention rejected product/README.md edit")
	}

	// Verify the remote repo got the committed story-map updates:
	// 1. shipped-card is now done with PR note
	// 2. r1 is now released
	// 3. follow-up was created
	// 4. product/README.md stayed untouched
	files := repo.Files()
	if !strings.Contains(files["product/README.md"], "Make civic bills legible") {
		t.Fatalf("product/README.md must remain untouched: %q", files["product/README.md"])
	}
	loaded, err := storymap.Load(a.Workspaces.WorkDir("sandbox"), "core")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("honed map has problems: %v", loaded.Problems)
	}
	if r1 := loaded.Map.Release("r1"); r1 == nil || r1.Status != "released" {
		t.Fatalf("expected r1 released, got %+v", r1)
	}
	byID := map[string]storymap.Task{}
	for _, tk := range loaded.Tasks {
		byID[tk.ID] = tk
	}
	if byID["shipped-card"].Status != "done" || !strings.Contains(byID["shipped-card"].OutcomeNote, "pull/42") {
		t.Fatalf("shipped-card not closed cleanly: %+v", byID["shipped-card"])
	}
	if byID["follow-up"].Status != "todo" || len(byID["follow-up"].DependsOn) != 1 || byID["follow-up"].DependsOn[0] != "shipped-card" {
		t.Fatalf("follow-up not added cleanly: %+v", byID["follow-up"])
	}
}
