package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/store"
)

func setupImprovementApp(t *testing.T, handler func(reqBody string) string) *App {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reply := handler(string(body))
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": reply}}},
			"usage":   map[string]int{"prompt_tokens": 300, "completion_tokens": 120},
		})
	}))
	t.Cleanup(upstream.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg, _, err = LoadStoredConfig(context.Background(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Env.LLM.Upstreams["home"] = config.Upstream{BaseURL: upstream.URL}
	cfg.Catalog.Models["gpt-6.1-sol"].InputPer1M = 1.0
	cfg.Catalog.Models["gpt-6.1-sol"].OutputPer1M = 2.0

	h := hub.New()
	eng := engine.New(cfg, st, nil, h, nil)
	a := &App{Cfg: cfg, Store: st, Engine: eng, Hub: h, LLM: llm.New(cfg)}
	if _, err := a.EditConfig(context.Background(), true,
		ConfigEdit{Section: "settings", Name: "planner", YAML: "model: gpt-6.1-sol\nstream: false\n"},
		ConfigEdit{Section: "settings", Name: "improvement", YAML: "auto_grade: true\nmodel: gpt-6.1-sol\nimprover_model: gpt-6.1-sol\nmin_failures: 2\n"},
	); err != nil {
		t.Fatal(err)
	}
	return a
}

func seedFinishedRun(t *testing.T, a *App, name string, status string) *store.Run {
	t.Helper()
	ctx := context.Background()
	src := fmt.Sprintf(`apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: %s, project: sandbox }
spec:
  start: review
  nodes:
    review:
      uses: preset/code-review
      model: gpt-6.1-sol
      next: { approve: $success, changes: $fail }
`, name)
	if _, err := a.Store.SaveFlow(ctx, &store.FlowVersion{Name: name, Project: "sandbox", YAML: src}); err != nil {
		t.Fatal(err)
	}
	run, err := a.Engine.CreateRun(ctx, name, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.AddVisit(ctx, run.ID, "review", "llm"); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetRunState(ctx, run, status, "", store.Now()); err != nil {
		t.Fatal(err)
	}
	fresh, err := a.Store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func TestImprovementLoopEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var improverPrompts []string
	proposeText := ""

	a := setupImprovementApp(t, func(body string) string {
		if strings.Contains(body, "prompt-improvement analyst") {
			mu.Lock()
			improverPrompts = append(improverPrompts, body)
			nextText := proposeText
			mu.Unlock()
			out, _ := json.Marshal(map[string]any{
				"action":     "propose",
				"theme":      "missing-line-citations",
				"run_ids":    regexp.MustCompile(`r-[0-9a-z-]+`).FindAllString(body, -1),
				"title":      "Require citing concrete file lines in code-review",
				"reason":     "Multiple runs failed instruction_adherence by omitting file paths.",
				"after_text": nextText,
			})
			return string(out)
		}
		// Grader response: efficiency passes, instruction_adherence fails (agent), unmentioned_workarounds fails (environment).
		out, _ := json.Marshal(map[string]any{
			"grades": []map[string]any{
				{"rubric": "efficiency", "pass": true, "reason": "Single focused pass."},
				{"rubric": "instruction_adherence", "pass": false, "blame": "agent", "theme": "missing-line-citations", "target_kind": "preset", "target": "code-review", "node": "review", "reason": "Review omitted file:line citations."},
				{"rubric": "unmentioned_workarounds", "pass": false, "blame": "environment", "theme": "pod-evicted", "target_kind": "infra", "target": "kubernetes", "node": "review", "reason": "Pod was evicted mid-step by descheduler."},
			},
		})
		return string(out)
	})

	ctx := context.Background()
	origPrompt := strings.TrimSpace(a.Cfg.Current().Catalog.Presets["code-review"].Prompt)
	proposeText = origPrompt + "\nAlways include file:line citations for every finding."

	r1 := seedFinishedRun(t, a, "f-one", store.RunFailed)
	r2 := seedFinishedRun(t, a, "f-two", store.RunSucceeded)

	// Grade both runs.
	g1, err := a.GradeRunSync(ctx, r1.ID)
	if err != nil {
		t.Fatalf("grade r1: %v", err)
	}
	if len(g1) != 4 {
		t.Fatalf("expected 4 grades, got %d", len(g1))
	}
	if g1[0].PromptVersions["preset:code-review"] != promptHash(origPrompt) {
		t.Fatalf("missing pinned prompt version hash: %+v", g1[0].PromptVersions)
	}
	if _, err := a.GradeRunSync(ctx, r2.ID); err != nil {
		t.Fatalf("grade r2: %v", err)
	}

	// Summary separates agent vs environment clusters and shows 1 eligible agent cluster.
	sum, err := a.ImprovementSummary(ctx, "sandbox")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Stats.GradedRuns != 2 || sum.Stats.AgentFailures != 2 || sum.Stats.EnvironmentFailures != 2 {
		t.Fatalf("unexpected stats: %+v", sum.Stats)
	}
	if len(sum.AgentClusters) != 1 || !sum.AgentClusters[0].Eligible {
		t.Fatalf("expected 1 eligible agent cluster: %+v", sum.AgentClusters)
	}
	if len(sum.EnvironmentClusters) != 1 || sum.EnvironmentClusters[0].Theme != "pod-evicted" {
		t.Fatalf("expected 1 environment cluster: %+v", sum.EnvironmentClusters)
	}

	// Run improver: drafts 1 pending proposal for preset:code-review (and ignores environment failures).
	res, err := a.RunImprover(ctx, ImproverOptions{Project: "sandbox", Trigger: "manual"})
	if err != nil {
		t.Fatalf("run improver: %v", err)
	}
	if res.ProposalsCreated != 1 || len(res.ProposalIDs) != 1 {
		t.Fatalf("expected 1 proposal created, got %+v", res)
	}
	propID := res.ProposalIDs[0]

	// Attention inbox surfaces the pending proposal.
	attn, err := a.Attention(ctx, "sandbox")
	if err != nil {
		t.Fatalf("attention: %v", err)
	}
	if attn.Counts.Proposals != 1 || len(attn.Proposals) != 1 || attn.Proposals[0].ID != propID {
		t.Fatalf("expected pending proposal in attention inbox: %+v", attn)
	}

	// Applying the proposal updates preset:code-review, preserves other preset fields, and records a prompt_revision.
	applied, _, err := a.ApplyProposal(ctx, propID, "casey", "good catch", "", false)
	if err != nil {
		t.Fatalf("apply proposal: %v", err)
	}
	if applied.Status != store.ProposalApplied {
		t.Fatalf("expected applied status, got %s", applied.Status)
	}
	updatedPreset := a.Cfg.Current().Catalog.Presets["code-review"]
	if !strings.Contains(updatedPreset.Prompt, "Always include file:line citations") {
		t.Fatalf("preset prompt not updated: %q", updatedPreset.Prompt)
	}
	if len(updatedPreset.Outcomes) == 0 {
		t.Fatal("applying proposal clobbered preset outcomes")
	}
	revs, err := a.Store.ListPromptRevisions(ctx, TargetPreset, "code-review", 10)
	if err != nil || len(revs) != 1 || revs[0].ProposalID != propID || revs[0].PrevHash != promptHash(origPrompt) {
		t.Fatalf("expected prompt revision recorded, got %+v (err=%v)", revs, err)
	}

	// Historical run r1 can still be inspected even after preset changed (no config drift error).
	rec, err := a.runRecord(ctx, r1)
	if err != nil || !strings.Contains(rec, origPrompt[:40]) {
		t.Fatalf("historical runRecord failed after preset edit: err=%v", err)
	}

	// Seed two more runs on the new prompt version, reject a proposal, and verify the next improver sees the rejection note.
	r3 := seedFinishedRun(t, a, "f-three", store.RunSucceeded)
	r4 := seedFinishedRun(t, a, "f-four", store.RunSucceeded)
	if _, err := a.GradeRunSync(ctx, r3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GradeRunSync(ctx, r4.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	proposeText = updatedPreset.Prompt + "\nSecond iteration edit."
	mu.Unlock()
	res2, err := a.RunImprover(ctx, ImproverOptions{Project: "sandbox"})
	if err != nil || res2.ProposalsCreated != 1 {
		t.Fatalf("second improver pass: res=%+v err=%v", res2, err)
	}
	rej, err := a.RejectProposal(ctx, res2.ProposalIDs[0], "casey", "too verbose for reviews")
	if err != nil || rej.Status != store.ProposalRejected {
		t.Fatalf("reject proposal: %+v err=%v", rej, err)
	}

	// Verify target version history shows BOTH prompt versions!
	sum2, err := a.ImprovementSummary(ctx, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	foundTwoVersions := false
	for _, tv := range sum2.TargetVersions {
		if tv.TargetKind == TargetPreset && tv.Target == "code-review" && len(tv.Versions) == 2 {
			foundTwoVersions = true
		}
	}
	if !foundTwoVersions {
		t.Fatalf("expected 2 tracked versions for preset:code-review, got %+v", sum2.TargetVersions)
	}

	// One more cycle: verify rejected proposal note is passed into the improver prompt!
	r5 := seedFinishedRun(t, a, "f-five", store.RunSucceeded)
	r6 := seedFinishedRun(t, a, "f-six", store.RunSucceeded)
	if _, err := a.GradeRunSync(ctx, r5.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GradeRunSync(ctx, r6.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunImprover(ctx, ImproverOptions{Project: "sandbox"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	lastPrompt := improverPrompts[len(improverPrompts)-1]
	mu.Unlock()
	if !strings.Contains(lastPrompt, "too verbose for reviews") {
		t.Fatalf("expected rejected proposal note in improver prompt:\n%s", lastPrompt)
	}
}

func TestApplyProposalRejectsStaleTargetWithoutForce(t *testing.T) {
	a := setupImprovementApp(t, func(string) string { return "{}" })
	ctx := context.Background()
	p := &store.ImprovementProposal{
		ID: "imp-stale", Status: store.ProposalPending, TargetKind: TargetPreset, Target: "code-review",
		Title: "Stale edit", Reason: "test", BeforeText: "different text", AfterText: "new text",
		BeforeHash: promptHash("different text"), AfterHash: promptHash("new text"),
	}
	if err := a.Store.CreateProposal(ctx, p); err != nil {
		t.Fatal(err)
	}
	_, _, err := a.ApplyProposal(ctx, p.ID, "casey", "", "", false)
	var conflict *ProposalConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ProposalConflictError, got %v", err)
	}
}

func TestDisagreedGradesExcludedFromImprover(t *testing.T) {
	a := setupImprovementApp(t, func(body string) string {
		out, _ := json.Marshal(map[string]any{
			"grades": []map[string]any{
				{"rubric": "efficiency", "pass": false, "blame": "agent", "theme": "spurious", "target_kind": "preset", "target": "code-review", "reason": "false positive"},
				{"rubric": "instruction_adherence", "pass": true, "reason": "ok"},
				{"rubric": "unmentioned_workarounds", "pass": true, "reason": "ok"},
			},
		})
		return string(out)
	})
	ctx := context.Background()
	r1 := seedFinishedRun(t, a, "d-one", store.RunSucceeded)
	r2 := seedFinishedRun(t, a, "d-two", store.RunSucceeded)
	g1, err := a.GradeRunSync(ctx, r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.GradeRunSync(ctx, r2.ID); err != nil {
		t.Fatal(err)
	}
	// Human disagrees with r1's efficiency grade, dropping cluster below min_failures=2.
	if _, err := a.DisagreeGrade(ctx, g1[0].ID, true, "casey", "legit work"); err != nil {
		t.Fatal(err)
	}
	res, err := a.RunImprover(ctx, ImproverOptions{Project: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClustersChecked != 0 || res.ProposalsCreated != 0 {
		t.Fatalf("disagreed grade should have kept cluster ineligible: %+v", res)
	}
}
