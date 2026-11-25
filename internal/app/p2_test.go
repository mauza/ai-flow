package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func TestTranscriptDigestDetectsRepeatedCommandsAndCacheHit(t *testing.T) {
	lines := []string{
		`{"type":"tool_execution_start","toolName":"read","args":{"path":"a.go"}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./..."}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./..."}}`,
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./..."}}`,
		`{"type":"message_end","message":{"usage":{"input":1000,"cacheRead":800}}}`,
	}
	digest := transcriptDigest([]byte(strings.Join(lines, "\n")))
	for _, want := range []string{"bash=3", "read=1", `REPEATED COMMANDS: 3x "go test ./..."`, "cache_hit=80%"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest %q missing %q", digest, want)
		}
	}
}

func TestSkillAndPresetConfigProposalsExportAndProtection(t *testing.T) {
	a := setupImprovementApp(t, func(string) string { return "{}" })
	ctx := context.Background()

	// 1. Apply a skill proposal.
	origSkill, ok := currentTargetText(a.Cfg.Current(), TargetSkill, "small-diffs")
	if !ok || origSkill == "" {
		t.Fatal("expected small-diffs skill in catalog")
	}
	newSkill := origSkill + "\n- Verify git diff --stat before finishing."
	pSkill := &store.ImprovementProposal{
		ID: "imp-skill", Status: store.ProposalPending, TargetKind: TargetSkill, Target: "small-diffs",
		Theme: "bloated-diff", Title: "Add diff stat check to small-diffs", Reason: "test",
		BeforeText: origSkill, AfterText: newSkill, BeforeHash: promptHash(origSkill), AfterHash: promptHash(newSkill),
	}
	if err := a.Store.CreateProposal(ctx, pSkill); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.ApplyProposal(ctx, pSkill.ID, "tester", "ok", "", false); err != nil {
		t.Fatalf("apply skill proposal: %v", err)
	}
	if !strings.Contains(a.Cfg.Current().Catalog.Skills["small-diffs"].Files["SKILL.md"], "git diff --stat") {
		t.Fatal("skill SKILL.md was not updated")
	}

	// 2. Apply a preset_config proposal.
	origCfg, ok := currentTargetText(a.Cfg.Current(), TargetPresetConfig, "code-review")
	if !ok {
		t.Fatal("expected code-review preset_config")
	}
	newCfg := "max_visits: 5\nmin_size: medium\n"
	pCfg := &store.ImprovementProposal{
		ID: "imp-pcfg", Status: store.ProposalPending, TargetKind: TargetPresetConfig, Target: "code-review",
		Theme: "exhausted-review", Title: "Raise code-review max_visits to 5", Reason: "test",
		BeforeText: origCfg, AfterText: newCfg, BeforeHash: promptHash(origCfg), AfterHash: promptHash(newCfg),
	}
	if err := a.Store.CreateProposal(ctx, pCfg); err != nil {
		t.Fatal(err)
	}
	origPrompt := a.Cfg.Current().Catalog.Presets["code-review"].Prompt
	if _, _, err := a.ApplyProposal(ctx, pCfg.ID, "tester", "ok", "", false); err != nil {
		t.Fatalf("apply preset_config proposal: %v", err)
	}
	gotPreset := a.Cfg.Current().Catalog.Presets["code-review"]
	if gotPreset.MaxVisits != 5 || gotPreset.Prompt != origPrompt {
		t.Fatalf("preset_config edit unexpected: max_visits=%d promptPreserved=%v", gotPreset.MaxVisits, gotPreset.Prompt == origPrompt)
	}

	// 3. Export applied improvements for GitOps sync.
	exported, err := a.ExportAppliedImprovements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"small-diffs:", "git diff --stat", "code-review:", "max_visits: 5"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("export missing %q:\n%s", want, exported)
		}
	}

	// 4. Verify LoadStoredConfig protects applied proposals from unrelated file changes.
	dir := t.TempDir()
	b, err := os.ReadFile("../../examples/templates/config/fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	files1, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg1, _, err := LoadStoredConfig(ctx, st2, files1)
	if err != nil {
		t.Fatal(err)
	}
	app2 := &App{Cfg: cfg1, Store: st2, Hub: hub.New()}
	if _, err := app2.EditConfig(ctx, true, ConfigEdit{Section: "models", Name: "gpt-6-luna", YAML: "upstream: home\nmodel: gpt-6-luna\nnotes: improved-in-db\n"}); err != nil {
		t.Fatal(err)
	}
	// Mark models/gpt-6-luna as protected by an applied improvement proposal.
	if err := st2.SetKV(ctx, kvImpApplied+"models/gpt-6-luna", `{"proposal_id":"imp-1"}`); err != nil {
		t.Fatal(err)
	}
	// File changes gpt-6-luna to something else -> protected DB version must survive!
	edited := strings.Replace(string(b), "notes: Alternate alias", "notes: file-overwrite-attempt", 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	files2, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, report, err := LoadStoredConfig(ctx, st2, files2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Catalog.Models["gpt-6-luna"].Notes != "improved-in-db" {
		t.Fatalf("protected DB entry was overwritten: %q", cfg2.Catalog.Models["gpt-6-luna"].Notes)
	}
	if len(report.Protected) != 1 || report.Protected[0] != "models gpt-6-luna" {
		t.Fatalf("expected protected report, got %+v", report)
	}
}

func TestCohensKappaCalibrationAndPrimitiveBacklog(t *testing.T) {
	a := setupImprovementApp(t, func(string) string {
		out, _ := json.Marshal(map[string]any{
			"grades": []map[string]any{
				{"rubric": "efficiency", "pass": true, "reason": "ok"},
				{"rubric": "instruction_adherence", "pass": false, "blame": "agent", "theme": "missing-primitive", "target_kind": "primitive", "target": "cache-pvc", "reason": "needs cache pvc"},
				{"rubric": "unmentioned_workarounds", "pass": true, "reason": "ok"},
				{"rubric": "change_quality", "pass": true, "reason": "clean"},
			},
		})
		return string(out)
	})
	ctx := context.Background()

	// Seed 2 runs and grade them, plus attach a retro with a primitive suggestion.
	r1 := seedFinishedRun(t, a, "k-one", store.RunSucceeded)
	r2 := seedFinishedRun(t, a, "k-two", store.RunFailed)
	g1, err := a.GradeRunSync(ctx, r1.ID)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := a.GradeRunSync(ctx, r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm both efficiency (pass) and instruction_adherence (fail) on r1 and r2 -> perfect agreement on efficiency & instruction_adherence.
	if _, err := a.ConfirmGrade(ctx, g1[0].ID, "casey", "agree"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConfirmGrade(ctx, g2[0].ID, "casey", "agree"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ConfirmGrade(ctx, g1[1].ID, "casey", "agree"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisagreeGrade(ctx, g2[1].ID, true, "casey", "disagree"); err != nil {
		t.Fatal(err)
	}

	// Store a retro with a primitive suggestion.
	retroBytes, _ := json.Marshal(&Retro{
		Status: "done", FinishedAt: store.Now(),
		Report: &RetroReport{
			Summary: "needs cache",
			Suggestions: []RetroSuggestion{
				{Kind: "primitive", Target: "cache-pvc", Change: "Mount shared Go build cache PVC", Why: "slow rebuilds", Priority: "high"},
			},
		},
	})
	if err := a.Store.SetKV(ctx, kvRetro(r1.ID), string(retroBytes)); err != nil {
		t.Fatal(err)
	}

	sum, err := a.ImprovementSummary(ctx, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	var eff *RubricStats
	for i := range sum.Rubrics {
		if sum.Rubrics[i].Name == "efficiency" {
			eff = &sum.Rubrics[i]
		}
	}
	if eff == nil || eff.Audited != 2 || eff.Confirmed != 2 || eff.CohensKappa == nil || *eff.CohensKappa != 1.0 {
		t.Fatalf("unexpected efficiency calibration: %+v", eff)
	}
	if len(sum.PrimitiveBacklog) == 0 {
		t.Fatalf("expected primitive backlog populated from grades + retros, got empty")
	}
}
