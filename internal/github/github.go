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

func (e *APIError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, strings.TrimSpace(e.Body)) }

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

// Checks returns every check run and commit status reported for ref.
func (c *Client) Checks(ctx context.Context, r Repo, ref string) ([]Check, error) {
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"check_runs"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", r.Owner, r.Name, url.PathEscape(ref)), nil, &runs); err != nil {
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
