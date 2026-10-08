package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

// configDir copies the template config into a directory the test can edit.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile("../../examples/templates/config/fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0o644)
	return dir
}

func editFile(t *testing.T, dir, old, new string) {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), old) {
		t.Fatalf("fixture lacks %q", old)
	}
	os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
}

func start(t *testing.T, st *store.Store, dir string) (*app.App, *app.StoredConfig) {
	t.Helper()
	files, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, stored, err := app.LoadStoredConfig(t.Context(), st, files)
	if err != nil {
		t.Fatal(err)
	}
	return &app.App{Cfg: cfg, Store: st, Hub: hub.New()}, stored
}

// A release bumps runtime images in the config files; that must reach a
// database-owned catalog without undoing edits made in the UI.
func TestFileChangesReachTheStoredCatalog(t *testing.T) {
	dir := configDir(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, stored := start(t, st, dir)
	if !stored.Seeded {
		t.Fatal("first start seeds")
	}
	if _, err := a.EditConfig(t.Context(), app.ConfigEdit{Section: "models", Name: "gpt-6-luna", YAML: "upstream: home\nmodel: gpt-6-luna\nnotes: edited in the UI\n"}); err != nil {
		t.Fatal(err)
	}

	// Restart with unchanged files: nothing from the files is applied.
	a, stored = start(t, st, dir)
	if len(stored.FromFiles) != 0 || a.Cfg.Current().Catalog.Models["gpt-6-luna"].Notes != "edited in the UI" {
		t.Fatalf("unchanged files: %+v", stored)
	}

	// A release bumps the runtime image and changes the project's budget.
	editFile(t, dir, "agent-go:validation-only", "agent-go:v2")
	editFile(t, dir, "budget: { usd_per_run: 4 }", "budget: { usd_per_run: 6 }")
	a, stored = start(t, st, dir)
	cfg := a.Cfg.Current()
	if got := strings.Join(stored.FromFiles, ","); got != "runtimes fixture-go,projects template-project" {
		t.Fatalf("from files: %s", got)
	}
	if cfg.Catalog.Runtimes["fixture-go"].Image != "example.invalid/replace-me/agent-go:v2" || cfg.Projects["template-project"].Spec.Budget.USDPerRun != 6 {
		t.Fatal("file changes not applied")
	}
	if cfg.Catalog.Models["gpt-6-luna"].Notes != "edited in the UI" {
		t.Fatal("a UI edit to an entry the files did not change was lost")
	}

	// When both changed the same entry, the files win.
	editFile(t, dir, "notes: Alternate alias", "notes: From the files. Alternate alias")
	a, _ = start(t, st, dir)
	if !strings.HasPrefix(a.Cfg.Current().Catalog.Models["gpt-6-luna"].Notes, "From the files.") {
		t.Fatal("the files' change to an entry must win")
	}
	// And a later restart does not re-apply it over a new UI edit.
	a.EditConfig(t.Context(), app.ConfigEdit{Section: "models", Name: "gpt-6-luna", YAML: "upstream: home\nmodel: gpt-6-luna\nnotes: edited again\n"})
	a, stored = start(t, st, dir)
	if len(stored.FromFiles) != 0 || a.Cfg.Current().Catalog.Models["gpt-6-luna"].Notes != "edited again" {
		t.Fatalf("re-applied: %+v", stored)
	}
}

func TestFileChangesThatConflictAreReported(t *testing.T) {
	dir := configDir(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := start(t, st, dir)
	// The UI points the planner at Luna and deletes Sol...
	if _, err := a.EditConfig(t.Context(),
		app.ConfigEdit{Section: "settings", Name: "planner", YAML: "model: gpt-6-luna\nstream: true\n"},
		app.ConfigEdit{Section: "models", Name: "gpt-6.1-sol"},
	); err != nil {
		t.Fatal(err)
	}
	// ...while the files change the planner settings, which still name Sol.
	editFile(t, dir, "stream: true", "stream: false")
	a, stored := start(t, st, dir)
	if !strings.Contains(stored.Error, "settings planner") || a.FilesError(t.Context()) == "" {
		t.Fatalf("want a reported conflict, got %+v", stored)
	}
	if a.Cfg.Current().Catalog.Planner.Model != "gpt-6-luna" {
		t.Fatal("on a conflict the database version is served")
	}
}
