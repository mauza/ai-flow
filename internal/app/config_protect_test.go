package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

// An applied improvement keeps the fields it set and nothing else: every other
// change to the same entry in the config files goes live.
func TestAppliedImprovementsProtectOnlyTheirFields(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile("../../examples/templates/config/fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	path := filepath.Join(t.TempDir(), "config.yaml")
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &App{Store: st, Hub: hub.New()}
	var report *StoredConfig
	restart := func() {
		t.Helper()
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		files, err := config.Load(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if a.Cfg, report, err = LoadStoredConfig(ctx, st, files); err != nil {
			t.Fatal(err)
		}
		if report.Error != "" {
			t.Fatal(report.Error)
		}
	}
	edit := func(from, to string) {
		t.Helper()
		if strings.Count(src, from) != 1 {
			t.Fatalf("%d of %q in the fixture", strings.Count(src, from), from)
		}
		src = strings.Replace(src, from, to, 1)
		restart()
	}
	apply := func(id, kind, target, after string) {
		t.Helper()
		before, _ := currentTargetText(a.Cfg.Current(), kind, target)
		p := &store.ImprovementProposal{ID: id, Status: store.ProposalPending, TargetKind: kind, Target: target, Title: id,
			BeforeText: before, AfterText: after, BeforeHash: promptHash(before), AfterHash: promptHash(after)}
		if err := st.CreateProposal(ctx, p); err != nil {
			t.Fatal(err)
		}
		if _, _, err := a.ApplyProposal(ctx, id, "me", "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	project := func() *config.Project { return a.Cfg.Current().Projects["template-project"] }
	restart()
	apply("p1", TargetProjectAgent, "template-project", "Be brief.")
	apply("p2", TargetPlanner, "guidance", "Plan small.")

	// Unrelated fields of the same entries still follow the files.
	edit("  start: manual\n", "  start: manual\n  schedules: [{ name: s, every: 1h, enabled: false, task: { title: T } }]\n")
	if len(project().Spec.Schedules) != 1 || project().Spec.Agent.Instructions != "Be brief." {
		t.Fatalf("project: %+v", project().Spec)
	}
	edit("planner:\n  model: gpt-6.1-sol\n", "planner:\n  model: gpt-6-luna\n")
	if p := a.Cfg.Current().Catalog.Planner; p.Model != "gpt-6-luna" || p.Guidance != "Plan small." {
		t.Fatalf("planner: %+v", p)
	}
	if a.FilesError(ctx) != "" || len(report.Protected) != 0 {
		t.Fatalf("no conflict yet: %q %v", a.FilesError(ctx), report.Protected)
	}

	// The files change the protected field: the applied value stands, visibly,
	// also after further restarts.
	edit("  start: manual\n", "  start: manual\n  agent: { instructions: Be thorough. }\n")
	restart()
	if project().Spec.Agent.Instructions != "Be brief." || !strings.Contains(a.FilesError(ctx), "projects/template-project") {
		t.Fatalf("conflict: %q, error %q", project().Spec.Agent.Instructions, a.FilesError(ctx))
	}

	// Putting the applied value in the files settles it; they lead again.
	edit("Be thorough.", "Be brief.")
	if a.FilesError(ctx) != "" {
		t.Fatalf("still reported: %q", a.FilesError(ctx))
	}
	edit("Be brief.", "Be careful.")
	if got := project().Spec.Agent.Instructions; got != "Be careful." {
		t.Fatalf("files no longer lead: %q", got)
	}
}
