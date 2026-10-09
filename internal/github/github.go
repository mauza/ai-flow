// Package github is the small slice of the GitHub REST API ai-flow needs:
// opening pull requests and reading repository context for the planner.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	api   string
	token string
	http  *http.Client
}

func New(apiURL, token string) *Client {
	return &Client{api: strings.TrimSuffix(apiURL, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// Repo identifies owner/name.
type Repo struct {
	Host, Owner, Name string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepoURL accepts https://host/owner/name(.git).
func ParseRepoURL(raw string) (Repo, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Repo{}, err
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if u.Host == "" || len(parts) != 2 {
		return Repo{}, fmt.Errorf("expected https://host/owner/repo, got %q", raw)
	}
	return Repo{Host: u.Host, Owner: parts[0], Name: parts[1]}, nil
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return &APIError{resp.StatusCode, string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
}

// OpenPR creates a pull request, or returns the open one for head if it exists
// (updating its title and body).
func (c *Client) OpenPR(ctx context.Context, r Repo, head, base, title, body string, draft bool) (*PullRequest, error) {
	var existing []PullRequest
	q := fmt.Sprintf("/repos/%s/%s/pulls?state=open&head=%s:%s", r.Owner, r.Name, url.QueryEscape(r.Owner), url.QueryEscape(head))
	if err := c.do(ctx, "GET", q, nil, &existing); err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		pr := existing[0]
		err := c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/pulls/%d", r.Owner, r.Name, pr.Number), map[string]any{"title": title, "body": body}, &pr)
		return &pr, err
	}
	var pr PullRequest
	err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/pulls", r.Owner, r.Name), map[string]any{
		"title": title, "head": head, "base": base, "body": body, "draft": draft,
	}, &pr)
	return &pr, err
}

// BranchExists reports whether a branch exists.
func (c *Client) BranchExists(ctx context.Context, r Repo, branch string) (bool, error) {
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/branches/%s", r.Owner, r.Name, url.PathEscape(branch)), nil, nil)
	if err == nil {
		return true, nil
	}
	if ae, ok := err.(*APIError); ok && ae.Status == 404 {
		return false, nil
	}
	return false, err
}

// Tree lists file paths on ref (recursive, truncated by GitHub for huge repos).
func (c *Client) Tree(ctx context.Context, r Repo, ref string) ([]string, error) {
	var out struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", r.Owner, r.Name, url.PathEscape(ref)), nil, &out); err != nil {
		return nil, err
	}
	var paths []string
	for _, t := range out.Tree {
		if t.Type == "blob" {
			paths = append(paths, t.Path)
		}
	}
	return paths, nil
}

// File returns a file's content on ref, or "" when it does not exist.
func (c *Client) File(ctx context.Context, r Repo, ref, path string) (string, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", r.Owner, r.Name, path, url.QueryEscape(ref)), nil, &out)
	if ae, ok := err.(*APIError); ok && ae.Status == 404 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if out.Encoding != "base64" {
		return out.Content, nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	return string(b), err
}

// BranchHead returns the commit SHA a branch points at.
func (c *Client) BranchHead(ctx context.Context, r Repo, branch string) (string, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/branches/%s", r.Owner, r.Name, url.PathEscape(branch)), nil, &b)
	return b.Commit.SHA, err
}

// PRState is the merge-relevant state of a pull request.
type PRState struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"` // open | closed
	Merged         bool   `json:"merged"`
	Mergeable      *bool  `json:"mergeable"` // nil while GitHub computes it
	MergeableState string `json:"mergeable_state"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (c *Client) PR(ctx context.Context, r Repo, number int) (*PRState, error) {
	var pr PRState
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", r.Owner, r.Name, number), nil, &pr)
	return &pr, err
}

// MergePR merges a pull request at headSHA (refusing if the head moved) and
// returns the merge commit.
func (c *Client) MergePR(ctx context.Context, r Repo, number int, method, headSHA string) (string, error) {
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	err := c.do(ctx, "PUT", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", r.Owner, r.Name, number), map[string]any{"merge_method": method, "sha": headSHA}, &out)
	if err == nil && !out.Merged {
		err = fmt.Errorf("github: merge of #%d not performed", number)
	}
	return out.SHA, err
}

// Check is one CI result on a commit: a check run or a commit status.
type Check struct {
	Name string
	Done bool
	OK   bool
	URL  string
	Note string // conclusion or state
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// Checks returns every check run and commit status reported for ref.
//
// Fine-grained personal access tokens cannot be granted the Checks
// permission, so when GitHub refuses check runs (403) the GitHub Actions jobs
// for the commit stand in for them: Actions reports each job as a check run
// of the same name, so name filters keep working. Checks from other CI
// providers are then not seen, but their commit statuses still are.
func (c *Client) Checks(ctx context.Context, r Repo, ref string) ([]Check, error) {
	var runs struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", r.Owner, r.Name, url.PathEscape(ref)), nil, &runs)
	if ae, ok := err.(*APIError); ok && ae.Status == 403 {
		runs.CheckRuns, err = c.actionsJobs(ctx, r, ref)
	}
	if err != nil {
		return nil, err
	}
	var status struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/status", r.Owner, r.Name, url.PathEscape(ref)), nil, &status); err != nil {
		return nil, err
	}
	var out []Check
	for _, cr := range runs.CheckRuns {
		ok := cr.Conclusion == "success" || cr.Conclusion == "neutral" || cr.Conclusion == "skipped"
		note := cr.Conclusion
		if note == "" {
			note = cr.Status
		}
		out = append(out, Check{Name: cr.Name, Done: cr.Status == "completed", OK: ok, URL: cr.HTMLURL, Note: note})
	}
	for _, s := range status.Statuses {
		out = append(out, Check{Name: s.Context, Done: s.State != "pending", OK: s.State == "success", URL: s.TargetURL, Note: s.State})
	}
	return out, nil
}

// actionsJobs lists the latest attempt of every GitHub Actions job run for
// ref's commit, shaped like check runs.
func (c *Client) actionsJobs(ctx context.Context, r Repo, ref string) ([]checkRun, error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s", r.Owner, r.Name, url.PathEscape(ref)), nil, &commit); err != nil {
		return nil, err
	}
	var wf struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=100", r.Owner, r.Name, commit.SHA), nil, &wf); err != nil {
		return nil, err
	}
	var out []checkRun
	for _, run := range wf.WorkflowRuns {
		var jobs struct {
			Jobs []checkRun `json:"jobs"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100", r.Owner, r.Name, run.ID), nil, &jobs); err != nil {
			return nil, err
		}
		out = append(out, jobs.Jobs...)
	}
	return out, nil
}

// FirstParent returns a commit's first parent: for a merge commit, the tip
// of the branch it was merged into (what was deployed before the merge).
func (c *Client) FirstParent(ctx context.Context, r Repo, sha string) (string, error) {
	var commit struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s", r.Owner, r.Name, url.PathEscape(sha)), nil, &commit); err != nil {
		return "", err
	}
	if len(commit.Parents) == 0 {
		return "", fmt.Errorf("github: commit %s has no parent", sha)
	}
	return commit.Parents[0].SHA, nil
}

// DispatchWorkflow starts a workflow_dispatch run of workflow (a file name
// under .github/workflows) on ref.
func (c *Client) DispatchWorkflow(ctx context.Context, r Repo, workflow, ref string, inputs map[string]string) error {
	return c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", r.Owner, r.Name, url.PathEscape(workflow)),
		map[string]any{"ref": ref, "inputs": inputs}, nil)
}

// WorkflowRun is one run of a GitHub Actions workflow.
type WorkflowRun struct {
	ID         int64     `json:"id"`
	Status     string    `json:"status"`     // queued | in_progress | completed
	Conclusion string    `json:"conclusion"` // success | failure | cancelled | ...
	CreatedAt  time.Time `json:"created_at"`
	HTMLURL    string    `json:"html_url"`
}

// DispatchedRuns lists recent workflow_dispatch runs of workflow on branch,
// newest first.
func (c *Client) DispatchedRuns(ctx context.Context, r Repo, workflow, branch string) ([]WorkflowRun, error) {
	var out struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?event=workflow_dispatch&branch=%s&per_page=20",
		r.Owner, r.Name, url.PathEscape(workflow), url.QueryEscape(branch)), nil, &out)
	return out.Runs, err
}

func (c *Client) Run(ctx context.Context, r Repo, id int64) (*WorkflowRun, error) {
	var run WorkflowRun
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/actions/runs/%d", r.Owner, r.Name, id), nil, &run)
	return &run, err
}

// RepoInfo is a repository the token can see.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
	Archived      bool   `json:"archived"`
	PushedAt      string `json:"pushed_at"`
}

// Repos lists the repositories the token's user owns or collaborates on,
// most recently pushed first (up to 500).
func (c *Client) Repos(ctx context.Context) ([]RepoInfo, error) {
	var all []RepoInfo
	for page := 1; page <= 5; page++ {
		var batch []RepoInfo
		path := fmt.Sprintf("/user/repos?per_page=100&page=%d&sort=pushed&affiliation=owner,collaborator,organization_member", page)
		if err := c.do(ctx, "GET", path, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// Repository returns one repository's details.
func (c *Client) Repository(ctx context.Context, r Repo) (*RepoInfo, error) {
	var out RepoInfo
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s", r.Owner, r.Name), nil, &out)
	return &out, err
}

// TreeEntry is a file in a commit's tree.
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// Files lists the blobs under prefix in commit sha's tree.
func (c *Client) Files(ctx context.Context, r Repo, sha, prefix string) ([]TreeEntry, error) {
	var out struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/trees/%s?recursive=1", r.Owner, r.Name, sha), nil, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, fmt.Errorf("%s: the repository tree is too large for GitHub to list in one response", r)
	}
	var files []TreeEntry
	for _, e := range out.Tree {
		if e.Type == "blob" && strings.HasPrefix(e.Path, prefix) {
			files = append(files, e)
		}
	}
	return files, nil
}

// Blob returns a blob's content.
func (c *Client) Blob(ctx context.Context, r Repo, sha string) ([]byte, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/blobs/%s", r.Owner, r.Name, sha), nil, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return []byte(out.Content), nil
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
}

// FileChange is one file in a commit: Content nil deletes the path.
type FileChange struct {
	Path    string
	Content []byte
}

// ErrNotFastForward means the branch moved since the commit's parent was read.
var ErrNotFastForward = fmt.Errorf("the branch has new commits; pull first")

// CommitFiles commits changes on top of parent and moves branch to the new
// commit only if branch still points at parent (no force push). It returns
// the new commit's SHA.
func (c *Client) CommitFiles(ctx context.Context, r Repo, branch, parent, message string, author map[string]string, changes []FileChange) (string, error) {
	var parentCommit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/git/commits/%s", r.Owner, r.Name, parent), nil, &parentCommit); err != nil {
		return "", err
	}
	var entries []map[string]any
	for _, ch := range changes {
		e := map[string]any{"path": ch.Path, "mode": "100644", "type": "blob"}
		if ch.Content == nil {
			e["sha"] = nil
		} else {
			e["content"] = string(ch.Content)
		}
		entries = append(entries, e)
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/git/trees", r.Owner, r.Name), map[string]any{"base_tree": parentCommit.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	in := map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parent}}
	if author != nil {
		in["author"] = author
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/%s/git/commits", r.Owner, r.Name), in, &commit); err != nil {
		return "", err
	}
	err := c.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", r.Owner, r.Name, branch), map[string]any{"sha": commit.SHA, "force": false}, nil)
	if ae, ok := err.(*APIError); ok && ae.Status == 422 && strings.Contains(strings.ToLower(ae.Body), "fast forward") {
		return "", ErrNotFastForward
	}
	if err != nil {
		return "", err
	}
	return commit.SHA, nil
}
