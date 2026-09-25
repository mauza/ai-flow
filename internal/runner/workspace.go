package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mauza/ai-flow/internal/protocol"
)

// git runs a git command in dir. The grant token is passed per command and
// never written to .git/config, so tools run by the agent can't pick it up.
func (r *Runner) git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "http.extraHeader=Authorization: Bearer " + r.b.Grant, "-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (r *Runner) prepareRepo(ctx context.Context) (string, error) {
	ra := r.b.Repo
	dir := filepath.Join(r.work, "repo")
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Retried pod on a reused volume: start clean.
		os.RemoveAll(dir)
	}
	if _, err := r.git(ctx, r.work, "clone", "--quiet", "--no-tags", ra.CloneURL, dir); err != nil {
		return "", err
	}
	for _, kv := range [][2]string{{"user.name", ra.AuthorName}, {"user.email", ra.AuthorEmail}, {"advice.detachedHead", "false"}} {
		if _, err := r.git(ctx, dir, "config", kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if _, err := r.git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+ra.Branch); err == nil {
		_, err = r.git(ctx, dir, "checkout", "--quiet", "-B", ra.Branch, "origin/"+ra.Branch)
		return dir, err
	}
	if _, err := r.git(ctx, dir, "checkout", "--quiet", "-B", ra.Branch, "origin/"+ra.Base); err != nil {
		return "", fmt.Errorf("base branch %q: %w", ra.Base, err)
	}
	return dir, nil
}

func (r *Runner) commitAndPush(ctx context.Context, res *protocol.Result) (string, error) {
	status, err := r.git(ctx, r.repo, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := r.git(ctx, r.repo, "add", "-A"); err != nil {
			return "", err
		}
		subject := firstLine(res.Summary)
		if subject == "" {
			subject = "changes"
		}
		msg := fmt.Sprintf("%s: %s\n\nFlow-Run: %s\nFlow-Node: %s#%d\nFlow-Outcome: %s\n",
			r.b.Node, truncate(subject, 72), r.b.RunID, r.b.Node, r.b.Visit, res.Outcome)
		if _, err := r.git(ctx, r.repo, "commit", "--quiet", "--no-verify", "-m", msg); err != nil {
			return "", err
		}
	}
	// Push whenever the local branch is ahead (the agent may have committed itself).
	ahead := true
	if _, err := r.git(ctx, r.repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+r.b.Repo.Branch); err == nil {
		n, _ := r.git(ctx, r.repo, "rev-list", "--count", "origin/"+r.b.Repo.Branch+"..HEAD")
		ahead = n != "0"
	} else {
		n, _ := r.git(ctx, r.repo, "rev-list", "--count", "origin/"+r.b.Repo.Base+"..HEAD")
		ahead = n != "0"
	}
	sha, err := r.git(ctx, r.repo, "rev-parse", "HEAD")
	if err != nil || !ahead {
		return "", err
	}
	if _, err := r.git(ctx, r.repo, "push", "--quiet", "origin", "HEAD:refs/heads/"+r.b.Repo.Branch); err != nil {
		return "", err
	}
	return sha, nil
}

func (r *Runner) diffStat(ctx context.Context) (*protocol.DiffStat, error) {
	out, err := r.git(ctx, r.repo, "diff", "--numstat", "origin/"+r.b.Repo.Base+"...HEAD")
	if err != nil {
		return nil, err
	}
	d := &protocol.DiffStat{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		d.FilesChanged++
		a, _ := strconv.Atoi(f[0])
		rm, _ := strconv.Atoi(f[1])
		d.LinesAdded += a
		d.LinesRemoved += rm
	}
	d.LinesChanged = d.LinesAdded + d.LinesRemoved
	return d, nil
}

// branchDiff returns the diff of the run branch against its base, truncated.
func (r *Runner) branchDiff(ctx context.Context, max int) string {
	if r.repo == "" {
		return ""
	}
	out, err := r.git(ctx, r.repo, "diff", "origin/"+r.b.Repo.Base+"...HEAD")
	if err != nil {
		return ""
	}
	if len(out) > max {
		return out[:max] + "\n… (diff truncated)"
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
