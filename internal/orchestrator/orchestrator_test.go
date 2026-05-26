package orchestrator

import (
	"testing"

	internalgit "github.com/mauza/ai-flow/internal/git"
	"github.com/mauza/ai-flow/internal/linear"
)

func TestResolveRepoConfigUsesFallbackOwnerForBareRepo(t *testing.T) {
	details := &linear.IssueDetails{
		Identifier:  "ENG-123",
		Description: "---\ngithub_repo: api\ndefault_branch: develop\n---\nUpdate the API",
	}

	repo, branch, err := resolveRepoConfig(details, "acme")
	if err != nil {
		t.Fatalf("resolveRepoConfig returned error: %v", err)
	}
	if repo != "acme/api" {
		t.Fatalf("repo = %q, want %q", repo, "acme/api")
	}
	if branch != "develop" {
		t.Fatalf("branch = %q, want %q", branch, "develop")
	}
}

func TestResolveRepoConfigEmptyBranchWhenNotSpecified(t *testing.T) {
	details := &linear.IssueDetails{
		Identifier:  "ENG-123",
		Description: "---\ngithub_repo: weave-lab/data-access\n---\nAdd a critical path test",
	}

	repo, branch, err := resolveRepoConfig(details, "weave-lab")
	if err != nil {
		t.Fatalf("resolveRepoConfig returned error: %v", err)
	}
	if repo != "weave-lab/data-access" {
		t.Fatalf("repo = %q, want %q", repo, "weave-lab/data-access")
	}
	// Branch should be empty — the orchestrator resolves it live via GitHub API.
	if branch != "" {
		t.Fatalf("branch = %q, want empty (resolved at runtime)", branch)
	}
}

func TestResolveRepoConfigAllowsFallbackAfterMissingRepo(t *testing.T) {
	details := &linear.IssueDetails{
		Identifier:  "ENG-123",
		Description: "Please update backend-platform rate limiting behavior",
	}

	_, _, err := resolveRepoConfig(details, "acme")
	if err == nil {
		t.Fatal("resolveRepoConfig returned nil error, want missing repo error")
	}
	if err.Error() != "issue ENG-123: github_repo is required" {
		t.Fatalf("error = %q, want missing repo error", err.Error())
	}
}

func TestMatchRepoFromText(t *testing.T) {
	repos := []internalgit.RepoInfo{
		{Name: "api", NameWithOwner: "acme/api"},
		{Name: "dashboard", NameWithOwner: "acme/dashboard"},
		{Name: "backend-platform", NameWithOwner: "acme/backend-platform"},
	}

	repo := matchRepoFromText("Please update backend-platform rate limiting behavior", "acme", repos)
	if repo != "acme/backend-platform" {
		t.Fatalf("repo = %q, want %q", repo, "acme/backend-platform")
	}

	repo = matchRepoFromText("Work in acme/dashboard on the settings page", "acme", repos)
	if repo != "acme/dashboard" {
		t.Fatalf("repo = %q, want %q", repo, "acme/dashboard")
	}
}

func TestMatchRepoFromTextNoConfidentMatch(t *testing.T) {
	repos := []internalgit.RepoInfo{
		{Name: "api", NameWithOwner: "acme/api"},
		{Name: "dashboard", NameWithOwner: "acme/dashboard"},
	}

	repo := matchRepoFromText("Fix customer login flow", "acme", repos)
	if repo != "" {
		t.Fatalf("repo = %q, want empty", repo)
	}
}

func TestMatchRepoFromTextAmbiguousMatch(t *testing.T) {
	repos := []internalgit.RepoInfo{
		{Name: "backend-api", NameWithOwner: "acme/backend-api"},
		{Name: "backend-web", NameWithOwner: "acme/backend-web"},
	}

	repo := matchRepoFromText("Fix backend auth handling", "acme", repos)
	if repo != "" {
		t.Fatalf("repo = %q, want empty for ambiguous match", repo)
	}
}
