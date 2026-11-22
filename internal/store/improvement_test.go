package store

import (
	"context"
	"testing"
)

func TestImprovementStoreLifecycleAndDeleteFlowCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, err := s.SaveFlow(ctx, &FlowVersion{Name: "demo-flow", YAML: "apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: {name: demo-flow}\nspec: {start: a, nodes: {a: {type: gate}}}\n"})
	check(t, err)
	check(t, s.CreateRun(ctx, &Run{ID: "r-1", FlowName: "demo-flow", FlowVersion: 1, Project: "p", Status: RunSucceeded}))

	facts := &RunFacts{
		RunID: "r-1", Project: "p", FlowName: "demo-flow", FlowVersion: 1, Status: RunSucceeded,
		WallMs: 5000, CostUSD: 0.42, Tokens: 12000, Visits: 3, LoopVisits: 1, MaxVisit: 2,
		PromptVersions: map[string]string{"preset:implement": "aaaaaaaaaa"},
		CreatedAt:      1000, FinishedAt: 6000,
	}
	check(t, s.UpsertRunFacts(ctx, facts))
	gotFacts, err := s.GetRunFacts(ctx, "r-1")
	check(t, err)
	if gotFacts.LoopVisits != 1 || gotFacts.PromptVersions["preset:implement"] != "aaaaaaaaaa" {
		t.Fatalf("unexpected facts: %+v", gotFacts)
	}

	g1 := &RunGrade{
		ID: "g-1", RunID: "r-1", Project: "p", FlowName: "demo-flow",
		Rubric: "efficiency", RubricVersion: "v1", Pass: false, Blame: BlameAgent,
		Theme: "blind-retry-loop", TargetKind: "preset", Target: "implement", Node: "work",
		Reason: "retried 4 times", PromptVersions: facts.PromptVersions, Model: "test",
		TokensIn: 100, TokensOut: 50, CostUSD: 0.01,
	}
	g2 := &RunGrade{
		ID: "g-2", RunID: "r-1", Project: "p", FlowName: "demo-flow",
		Rubric: "instruction_adherence", RubricVersion: "v1", Pass: true,
		Reason: "followed rules", PromptVersions: facts.PromptVersions, Model: "test",
	}
	check(t, s.UpsertRunGrades(ctx, []*RunGrade{g1, g2}))

	// Disagreement toggles cleanly.
	dis, err := s.SetGradeDisagreement(ctx, "g-1", true, "alice", "retries were needed")
	check(t, err)
	if !dis.Disagreed || dis.DisagreedBy != "alice" || dis.DisagreedNote != "retries were needed" {
		t.Fatalf("disagreement not persisted: %+v", dis)
	}
	unrev, err := s.ListRunGrades(ctx, GradeFilter{Unreviewed: true})
	check(t, err)
	if len(unrev) != 1 || unrev[0].ID != "g-2" {
		t.Fatalf("disagreed grade should be excluded from unreviewed: %+v", unrev)
	}

	// Re-grading replaces the row for (run_id, rubric) and clears disagreement.
	g1v2 := *g1
	g1v2.ID = "g-1-regrade"
	g1v2.Reason = "fresh reason"
	check(t, s.UpsertRunGrades(ctx, []*RunGrade{&g1v2}))
	allGrades, err := s.ListRunGrades(ctx, GradeFilter{RunID: "r-1"})
	check(t, err)
	if len(allGrades) != 2 {
		t.Fatalf("expected 2 grades after upsert, got %d", len(allGrades))
	}
	for _, g := range allGrades {
		if g.Rubric == "efficiency" && (g.ID != "g-1-regrade" || g.Disagreed) {
			t.Fatalf("upsert did not reset efficiency grade: %+v", g)
		}
	}

	// Mark reviewed.
	check(t, s.MarkGradesReviewed(ctx, []string{"g-1-regrade"}, 9999))
	gotG, err := s.GetRunGrade(ctx, "g-1-regrade")
	check(t, err)
	if gotG.ReviewedAt != 9999 {
		t.Fatalf("expected reviewed_at 9999, got %d", gotG.ReviewedAt)
	}

	// Proposals + prompt revisions.
	prop := &ImprovementProposal{
		ID: "imp-1", Status: ProposalPending, TargetKind: "preset", Target: "implement",
		Theme: "blind-retry-loop", Title: "Diagnose before retrying", Reason: "3 failures",
		BeforeText: "old", AfterText: "new", BeforeHash: "111", AfterHash: "222",
		EvidenceRuns: []string{"r-1"}, EvidenceGrades: []string{"g-1-regrade"},
	}
	check(t, s.CreateProposal(ctx, prop))
	dec, err := s.DecideProposal(ctx, "imp-1", ProposalApplied, "casey", "looks good", "edited", "333")
	check(t, err)
	if dec.Status != ProposalApplied || dec.AfterText != "edited" || dec.AfterHash != "333" {
		t.Fatalf("unexpected proposal decision: %+v", dec)
	}
	if _, err := s.DecideProposal(ctx, "imp-1", ProposalRejected, "casey", "too late", "", ""); err == nil {
		t.Fatal("expected error deciding non-pending proposal")
	}

	check(t, s.AddPromptRevision(ctx, &PromptRevision{
		TargetKind: "preset", Target: "implement", VersionHash: "333", PrevHash: "111",
		Author: "casey", Note: "applied imp-1", ProposalID: "imp-1",
	}))
	revs, err := s.ListPromptRevisions(ctx, "preset", "implement", 10)
	check(t, err)
	if len(revs) != 1 || revs[0].ProposalID != "imp-1" {
		t.Fatalf("unexpected revisions: %+v", revs)
	}

	// DeleteFlow cleans up run_facts and run_grades for deleted runs.
	_, err = s.DeleteFlow(ctx, "demo-flow")
	check(t, err)
	if _, err := s.GetRunFacts(ctx, "r-1"); err != ErrNotFound {
		t.Fatalf("expected run_facts deleted, got %v", err)
	}
	rem, err := s.ListRunGrades(ctx, GradeFilter{RunID: "r-1"})
	check(t, err)
	if len(rem) != 0 {
		t.Fatalf("expected run_grades deleted, got %d", len(rem))
	}
}
