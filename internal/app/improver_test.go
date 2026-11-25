package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/store"
)

// failGrade stores one failing, agent-blamed grade of a finished run.
func failGrade(t *testing.T, a *App, n int, theme string) *store.Run {
	t.Helper()
	run := seedFinishedRun(t, a, fmt.Sprintf("flow-%d", n), store.RunFailed)
	if err := a.Store.UpsertRunGrades(context.Background(), []*store.RunGrade{{ID: fmt.Sprintf("g-%d", n), RunID: run.ID, Project: "sandbox",
		Rubric: "efficiency", RubricVersion: "v", Blame: store.BlameAgent, Theme: theme, TargetKind: TargetPreset, Target: "code-review",
		Reason: "used far too many tokens", CreatedAt: store.Now()}}); err != nil {
		t.Fatal(err)
	}
	return run
}

// Themes are free text a model picks run by run. Production had 37 clusters of
// one run each, so nothing ever reached min_failures.
func TestDifferentThemesOfOneTargetFormOneCluster(t *testing.T) {
	reply, calls := map[string]any{"action": "skip"}, 0
	var prompt string
	a := setupImprovementApp(t, func(body string) string {
		calls++
		prompt = body
		out, _ := json.Marshal(reply)
		return string(out)
	})
	ctx := context.Background()
	runs := []*store.Run{failGrade(t, a, 1, "token-budget-overrun"), failGrade(t, a, 2, "excessive-token-use"), failGrade(t, a, 3, "unrelated-thing")}
	pass := func() *ImproverRunStatus {
		t.Helper()
		st, err := a.RunImprover(ctx, ImproverOptions{Trigger: "schedule"})
		if err != nil || st.Error != "" {
			t.Fatal(err, st)
		}
		return st
	}

	sum, err := a.ImprovementSummary(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.AgentClusters) != 1 || !sum.AgentClusters[0].Eligible || len(sum.AgentClusters[0].Themes) != 3 {
		t.Fatalf("clusters: %+v", sum.AgentClusters)
	}

	// No recurring cause: the grades stay, and it is not asked again until
	// something new fails.
	if st := pass(); st.SkippedClusters != 1 || calls != 1 {
		t.Fatalf("%+v after %d calls", st, calls)
	}
	if st := pass(); st.ClustersChecked != 0 || calls != 1 {
		t.Fatalf("asked again without new failures: %+v", st)
	}

	// A proposal must rest on enough of the runs it was shown.
	time.Sleep(2 * time.Millisecond)
	runs = append(runs, failGrade(t, a, 4, "token-cap-hit"))
	reply = map[string]any{"action": "propose", "theme": "token overrun", "run_ids": []string{runs[0].ID, "r-invented"}, "title": "T", "reason": "R", "after_text": "Use fewer tokens."}
	if st := pass(); st.ProposalsCreated != 0 {
		t.Fatalf("proposal with one real run: %+v", st)
	}
	time.Sleep(2 * time.Millisecond)
	runs = append(runs, failGrade(t, a, 5, "token-cap-hit"))
	cited := []string{runs[0].ID, runs[1].ID, runs[4].ID}
	reply["run_ids"] = cited
	if st := pass(); st.ProposalsCreated != 1 {
		t.Fatalf("%+v", st)
	}
	for _, r := range runs {
		if !strings.Contains(prompt, r.ID) {
			t.Fatalf("prompt lacks run %s", r.ID)
		}
	}
	props, _ := a.Store.ListProposals(ctx, store.ProposalFilter{})
	if len(props) != 1 || len(props[0].EvidenceRuns) != 3 || props[0].Theme != "token-overrun" {
		t.Fatalf("proposal: %+v", props[0])
	}
	// Only the cited failures are used up.
	sum, _ = a.ImprovementSummary(ctx, "")
	if len(sum.AgentClusters) != 1 || len(sum.AgentClusters[0].RunIDs) != 2 {
		t.Fatalf("left over: %+v", sum.AgentClusters)
	}
}

func TestPromptVersionsComeFromThePast(t *testing.T) {
	a := setupImprovementApp(t, func(string) string { return "{}" })
	ctx := context.Background()
	harness := TargetKey(TargetHarness, "pi")
	guidance := func(text string) {
		t.Helper()
		time.Sleep(2 * time.Millisecond)
		if _, err := a.EditConfig(ctx, true, ConfigEdit{Section: "harnesses", Name: "pi", YAML: "instructions: " + text + "\n"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	guidance("one")
	start := store.Now()
	time.Sleep(2 * time.Millisecond)
	guidance("two")
	if got := a.promptVersionAt(ctx, TargetHarness, "pi", start); got != promptHash("one") {
		t.Fatalf("%s at start: %q", harness, got)
	}
	if got := a.promptVersionAt(ctx, TargetHarness, "pi", store.Now()); got != promptHash("two") {
		t.Fatalf("%s now: %q", harness, got)
	}
	// Before the revision log, the version is unknown, not today's.
	if got := a.promptVersionAt(ctx, TargetHarness, "pi", 1); got != "" {
		t.Fatalf("invented a version: %q", got)
	}

	// Versions recorded the old way are replaced once.
	run := seedFinishedRun(t, a, "old", store.RunSucceeded)
	if _, err := a.RecordRunFacts(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetPromptVersions(ctx, run.ID, map[string]string{"planner:guidance": "wrong"}); err != nil {
		t.Fatal(err)
	}
	if err := a.repinPromptVersions(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := a.Store.GetRunFacts(ctx, run.ID)
	if err != nil || f.PromptVersions["planner:guidance"] == "wrong" || f.PromptVersions["preset:code-review"] == "" {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestBackfillHonorsSampling(t *testing.T) {
	a := setupImprovementApp(t, func(string) string { return `{"grades":[{"rubric":"efficiency","pass":true}]}` })
	ctx := context.Background()
	if _, err := a.EditConfig(ctx, true, ConfigEdit{Section: "settings", Name: "improvement", YAML: "model: gpt-6.1-sol\nsample_pct: 40\n"}); err != nil {
		t.Fatal(err)
	}
	want := 0
	for i := range 12 {
		if r := seedFinishedRun(t, a, fmt.Sprintf("s-%d", i), store.RunSucceeded); shouldSample(r.ID, 40) {
			want++
		}
	}
	queued, err := a.GradeUngradedRuns(ctx, "", 50, true)
	if err != nil || len(queued) != want {
		t.Fatalf("queued %d, want %d (%v)", len(queued), want, err)
	}
	for _, id := range queued {
		for a.Grading(id) {
			time.Sleep(5 * time.Millisecond)
		}
	}
}
