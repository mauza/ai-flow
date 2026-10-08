package config_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRevertDeployPresetFastForwards runs the revert-deploy preset's command
// against a real repository: a release branch merged into main with a merge
// commit. The run branch must end on main plus a revert, so the runner's
// plain (non-force) push is a fast-forward, and the release's change is gone.
func TestRevertDeployPresetFastForwards(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := loadCatalog(t)
	script := cfg.Catalog.Presets["revert-deploy"].Run
	dir := t.TempDir()
	git := func(wd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	origin := filepath.Join(dir, "origin.git")
	git(dir, "init", "--quiet", "--bare", "-b", "main", origin)
	seed := filepath.Join(dir, "seed")
	git(dir, "clone", "--quiet", origin, seed)
	os.WriteFile(filepath.Join(seed, "app.txt"), []byte("v1\n"), 0o644)
	git(seed, "add", ".")
	git(seed, "commit", "--quiet", "-m", "v1")
	git(seed, "push", "--quiet", "origin", "HEAD:main")
	git(seed, "checkout", "--quiet", "-b", "ai-flow/release")
	os.WriteFile(filepath.Join(seed, "app.txt"), []byte("v2 (broken)\n"), 0o644)
	git(seed, "commit", "--quiet", "-am", "release")
	git(seed, "push", "--quiet", "origin", "ai-flow/release")
	runHead := git(seed, "rev-parse", "HEAD")
	git(seed, "checkout", "--quiet", "main")
	git(seed, "merge", "--quiet", "--no-ff", "-m", "Merge release", "ai-flow/release")
	git(seed, "push", "--quiet", "origin", "main")
	merged := git(seed, "rev-parse", "HEAD")

	// The runner's workspace: a clone on the run branch.
	work := filepath.Join(dir, "work")
	git(dir, "clone", "--quiet", origin, work)
	git(work, "checkout", "--quiet", "-B", "ai-flow/release", "origin/ai-flow/release")
	cmd := exec.Command("bash", "-o", "pipefail", "-c", script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "AI_FLOW_INPUT_SHA="+merged, "AI_FLOW_INPUT_BASE=main",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("revert script: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(work, "app.txt")); string(got) != "v1\n" {
		t.Fatalf("release not reverted: %q", got)
	}
	git(work, "merge-base", "--is-ancestor", runHead, "HEAD") // fast-forward of the run branch
	git(work, "merge-base", "--is-ancestor", merged, "HEAD")  // and of main
	git(work, "push", "--quiet", "origin", "HEAD:refs/heads/ai-flow/release")
}
