package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Manager wraps git and gh CLI commands for repository operations.
type Manager struct{}

// NewManager creates a new git Manager after verifying that git and gh are available.
// Returns an error describing which tools are missing.
func NewManager() (*Manager, error) {
	var missing []string
	if _, err := exec.LookPath("git"); err != nil {
		missing = append(missing, "git")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		missing = append(missing, "gh")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required tools not found in PATH: %s", strings.Join(missing, ", "))
	}
	return &Manager{}, nil
}

// Clone performs a shallow clone of the given repo into dir.
func (m *Manager) Clone(ctx context.Context, repo, branch, dir string) error {
	url := "https://github.com/" + repo + ".git"
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, url, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// CreateBranch creates and checks out a new branch in the given directory.
func (m *Manager) CreateBranch(ctx context.Context, dir, name string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "-b", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout -b: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// FetchAndCheckout fetches a remote branch and checks it out locally.
func (m *Manager) FetchAndCheckout(ctx context.Context, dir, branch string) error {
	fetchCmd := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "origin", branch)
	if out, err := fetchCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %s: %w", strings.TrimSpace(string(out)), err)
	}

	checkoutCmd := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "-b", branch, "origin/"+branch)
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// BranchExistsOnRemote checks if a branch exists on the remote origin.
func (m *Manager) BranchExistsOnRemote(ctx context.Context, dir, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "ls-remote", "--heads", "origin", branch)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git ls-remote: %w", err)
	}
	return strings.TrimSpace(stdout.String()) != "", nil
}

// HasChanges returns true if the working tree has uncommitted changes.
func (m *Manager) HasChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(stdout.String()) != "", nil
}

// CommitAll stages all changes and commits with the given message.
func (m *Manager) CommitAll(ctx context.Context, dir, message string) error {
	addCmd := exec.CommandContext(ctx, "git", "-C", dir, "add", "-A")
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %s: %w", strings.TrimSpace(string(out)), err)
	}

	commitCmd := exec.CommandContext(ctx, "git", "-C", dir, "commit", "-m", message)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Push pushes the branch to origin with upstream tracking.
func (m *Manager) Push(ctx context.Context, dir, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "push", "-u", "origin", branch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// CreatePR creates a GitHub pull request using the gh CLI and returns the PR URL.
func (m *Manager) CreatePR(ctx context.Context, dir, title, body, base, head string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--title", title,
		"--body", body,
		"--base", base,
		"--head", head,
	)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		stderr := cmd.Stderr.(*bytes.Buffer).String()
		return "", fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(stderr), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Cleanup removes the temporary directory.
func (m *Manager) Cleanup(dir string) {
	os.RemoveAll(dir)
}

var nonAlphanumeric = regexp.MustCompile(`[^a-z0-9]+`)

// SanitizeBranchName creates a git-safe branch name from an issue identifier and title.
// e.g. "ENG-123" + "Fix auth bug" → "eng-123-fix-auth-bug"
func SanitizeBranchName(identifier, title string) string {
	raw := strings.ToLower(identifier + "-" + title)
	sanitized := nonAlphanumeric.ReplaceAllString(raw, "-")
	sanitized = strings.Trim(sanitized, "-")
	if len(sanitized) > 60 {
		sanitized = sanitized[:60]
		sanitized = strings.TrimRight(sanitized, "-")
	}
	return sanitized
}
