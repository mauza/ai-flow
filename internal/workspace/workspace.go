// Package workspace keeps a scratch checkout of a project's product/
// directory on the control plane's disk. Edits stay local until the user
// commits them; a commit goes straight to the project's base branch through
// the GitHub API and only if nobody pushed in between.
//
// On disk, per project:
//
//	<root>/<project>/state.json   the commit the checkout is at, and its blob SHAs
//	<root>/<project>/base/...     the files as of that commit
//	<root>/<project>/work/...     the files with local edits
package workspace

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mauza/ai-flow/internal/github"
)

// Prefix is the only directory a workspace holds and edits.
const Prefix = "product/"

// ErrConflict means a pull would overwrite local edits.
var ErrConflict = errors.New("conflict")

// Source is where a project's workspace comes from.
type Source struct {
	Repo   github.Repo
	Branch string
}

type state struct {
	Repo   string            `json:"repo"`
	Branch string            `json:"branch"`
	Head   string            `json:"head"`
	Files  map[string]string `json:"files"` // path → blob SHA at Head
	Pulled int64             `json:"pulled_at"`
}

// Change is a file that differs from the checked-out commit.
type Change struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // added | modified | deleted
	Base string `json:"base,omitempty"`
	Work string `json:"work,omitempty"`
}

// Status describes a workspace.
type Status struct {
	Repo     string   `json:"repo"`
	Branch   string   `json:"branch"`
	Head     string   `json:"head"`
	PulledAt int64    `json:"pulled_at"`
	Changes  []Change `json:"changes"`
}

// PullResult says what a pull changed.
type PullResult struct {
	Head      string   `json:"head"`
	Updated   []string `json:"updated"`
	Conflicts []string `json:"conflicts,omitempty"`
}

type Manager struct {
	root  string
	gh    *github.Client
	now   func() int64
	locks sync.Map // project → *sync.Mutex
}

func New(root string, gh *github.Client, now func() int64) *Manager {
	return &Manager{root: root, gh: gh, now: now}
}

func (m *Manager) lock(project string) func() {
	l, _ := m.locks.LoadOrStore(project, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) dir(project string) string { return filepath.Join(m.root, project) }

// WorkDir is the directory holding the edited files (paths start with product/).
func (m *Manager) WorkDir(project string) string { return filepath.Join(m.dir(project), "work") }

func (m *Manager) loadState(project string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(m.dir(project), "state.json"))
	if err != nil {
		return nil, err
	}
	var st state
	return &st, json.Unmarshal(b, &st)
}

func (m *Manager) saveState(project string, st *state) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(m.dir(project), "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dir(project), "state.json"))
}

// Ensure checks out the workspace if it does not exist yet (or the project
// now points at another repository or branch) and returns its status.
func (m *Manager) Ensure(ctx context.Context, project string, src Source) (*Status, error) {
	unlock := m.lock(project)
	defer unlock()
	if err := m.ensure(ctx, project, src); err != nil {
		return nil, err
	}
	return m.status(project)
}

func (m *Manager) ensure(ctx context.Context, project string, src Source) error {
	st, err := m.loadState(project)
	if err == nil && st.Repo == src.Repo.String() && st.Branch == src.Branch {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Fresh checkout, or the project now points elsewhere: start over, but
	// keep uncommitted edits of the old checkout aside instead of losing them.
	if st != nil {
		if changes, _ := m.changes(project, st); len(changes) > 0 {
			aside := fmt.Sprintf("%s.unsaved-%d", m.dir(project), m.now())
			if err := os.Rename(m.dir(project), aside); err != nil {
				return err
			}
			slog.Warn("workspace source changed; kept uncommitted edits aside", "project", project, "dir", aside)
		}
	}
	if err := os.RemoveAll(m.dir(project)); err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir(project), 0o755); err != nil {
		return err
	}
	_, err = m.pull(ctx, project, src, &state{Repo: src.Repo.String(), Branch: src.Branch, Files: map[string]string{}})
	return err
}

// Pull brings the workspace up to the branch head. Local edits are kept; if
// the remote changed a file that is also edited locally, nothing changes and
// the error is ErrConflict with the files listed in the result.
func (m *Manager) Pull(ctx context.Context, project string, src Source) (*PullResult, error) {
	unlock := m.lock(project)
	defer unlock()
	if err := m.ensure(ctx, project, src); err != nil {
		return nil, err
	}
	st, err := m.loadState(project)
	if err != nil {
		return nil, err
	}
	return m.pull(ctx, project, src, st)
}

func (m *Manager) pull(ctx context.Context, project string, src Source, st *state) (*PullResult, error) {
	head, err := m.gh.BranchHead(ctx, src.Repo, src.Branch)
	if err != nil {
		return nil, fmt.Errorf("reading %s %s: %w", src.Repo, src.Branch, err)
	}
	res := &PullResult{Head: head}
	if head == st.Head {
		return res, nil
	}
	entries, err := m.gh.Files(ctx, src.Repo, head, Prefix)
	if err != nil {
		return nil, err
	}
	remote := map[string]string{}
	for _, e := range entries {
		remote[e.Path] = e.SHA
	}
	edited, err := m.changes(project, st)
	if err != nil {
		return nil, err
	}
	local := map[string]bool{}
	for _, c := range edited {
		local[c.Path] = true
	}
	// Files the remote changed since our commit.
	var moved []string
	for p, sha := range remote {
		if st.Files[p] != sha {
			moved = append(moved, p)
		}
	}
	for p := range st.Files {
		if _, ok := remote[p]; !ok {
			moved = append(moved, p)
		}
	}
	sort.Strings(moved)
	for _, p := range moved {
		if local[p] {
			// Same content on both sides is not a conflict.
			if b, err := os.ReadFile(m.path(project, "work", p)); err == nil && BlobSHA(b) == remote[p] {
				continue
			}
			res.Conflicts = append(res.Conflicts, p)
		}
	}
	if len(res.Conflicts) > 0 {
		return res, ErrConflict
	}
	for _, p := range moved {
		sha, ok := remote[p]
		if !ok {
			os.Remove(m.path(project, "base", p))
			os.Remove(m.path(project, "work", p))
			res.Updated = append(res.Updated, p)
			continue
		}
		b, err := m.gh.Blob(ctx, src.Repo, sha)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if err := writeFile(m.path(project, "base", p), b); err != nil {
			return nil, err
		}
		if !local[p] {
			if err := writeFile(m.path(project, "work", p), b); err != nil {
				return nil, err
			}
		}
		res.Updated = append(res.Updated, p)
	}
	st.Head, st.Files, st.Pulled = head, remote, m.now()
	return res, m.saveState(project, st)
}

// Status lists the local changes of a checked-out workspace (see Ensure).
func (m *Manager) Status(project string) (*Status, error) {
	unlock := m.lock(project)
	defer unlock()
	return m.status(project)
}

func (m *Manager) status(project string) (*Status, error) {
	st, err := m.loadState(project)
	if err != nil {
		return nil, err
	}
	changes, err := m.changes(project, st)
	if err != nil {
		return nil, err
	}
	return &Status{Repo: st.Repo, Branch: st.Branch, Head: st.Head, PulledAt: st.Pulled, Changes: changes}, nil
}

// changes compares work against the checked-out commit (by blob SHA).
func (m *Manager) changes(project string, st *state) ([]Change, error) {
	out := []Change{} // never null in JSON
	seen := map[string]bool{}
	work := m.WorkDir(project)
	err := filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(work, p)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch sha, ok := st.Files[rel]; {
		case !ok:
			out = append(out, Change{Path: rel, Kind: "added", Work: string(b)})
		case sha != BlobSHA(b):
			base, _ := os.ReadFile(m.path(project, "base", rel))
			out = append(out, Change{Path: rel, Kind: "modified", Base: string(base), Work: string(b)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for p := range st.Files {
		if !seen[p] {
			base, _ := os.ReadFile(m.path(project, "base", p))
			out = append(out, Change{Path: p, Kind: "deleted", Base: string(base)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Read returns a file from the workspace (with local edits).
func (m *Manager) Read(project, p string) ([]byte, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	return os.ReadFile(m.path(project, "work", p))
}

// Write changes a file locally (nothing is committed).
func (m *Manager) Write(project, p string, content []byte) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	unlock := m.lock(project)
	defer unlock()
	return writeFile(m.path(project, "work", p), content)
}

// Remove deletes a file or directory locally.
func (m *Manager) Remove(project, p string) error {
	if err := CheckPath(strings.TrimSuffix(p, "/")); err != nil {
		return err
	}
	unlock := m.lock(project)
	defer unlock()
	return os.RemoveAll(m.path(project, "work", p))
}

// Discard drops local edits to paths (all edits when paths is empty).
func (m *Manager) Discard(project string, paths []string) error {
	unlock := m.lock(project)
	defer unlock()
	st, err := m.loadState(project)
	if err != nil {
		return err
	}
	changes, err := m.changes(project, st)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	for _, c := range changes {
		if len(paths) > 0 && !want[c.Path] {
			continue
		}
		if c.Kind == "added" {
			if err := os.Remove(m.path(project, "work", c.Path)); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(m.path(project, "base", c.Path))
		if err != nil {
			return err
		}
		if err := writeFile(m.path(project, "work", c.Path), b); err != nil {
			return err
		}
	}
	return nil
}

// Commit pushes every local change as one commit on the branch. It fails with
// github.ErrNotFastForward when the branch moved since the last pull.
func (m *Manager) Commit(ctx context.Context, project string, src Source, message string, author map[string]string) (string, error) {
	unlock := m.lock(project)
	defer unlock()
	st, err := m.loadState(project)
	if err != nil {
		return "", err
	}
	changes, err := m.changes(project, st)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", fmt.Errorf("nothing to commit")
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("a commit message is required")
	}
	var files []github.FileChange
	for _, c := range changes {
		f := github.FileChange{Path: c.Path}
		if c.Kind != "deleted" {
			f.Content = []byte(c.Work)
		}
		files = append(files, f)
	}
	sha, err := m.gh.CommitFiles(ctx, src.Repo, src.Branch, st.Head, message, author, files)
	if err != nil {
		return "", err
	}
	for _, c := range changes {
		if c.Kind == "deleted" {
			os.Remove(m.path(project, "base", c.Path))
			delete(st.Files, c.Path)
			continue
		}
		if err := writeFile(m.path(project, "base", c.Path), []byte(c.Work)); err != nil {
			return "", err
		}
		st.Files[c.Path] = BlobSHA([]byte(c.Work))
	}
	st.Head = sha
	return sha, m.saveState(project, st)
}

func (m *Manager) path(project, side, p string) string {
	return filepath.Join(m.dir(project), side, filepath.FromSlash(p))
}

// CheckPath accepts clean, relative paths inside product/.
func CheckPath(p string) error {
	if p == "" || path.Clean(p) != p || !strings.HasPrefix(p, Prefix) || strings.Contains(p, "\\") {
		return fmt.Errorf("path %q must be a clean path under %s", p, Prefix)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." || strings.HasPrefix(part, ".") && part != ".gitkeep" {
			return fmt.Errorf("path %q must not contain dot segments or hidden files", p)
		}
	}
	return nil
}

// BlobSHA is git's object id for a blob with this content.
func BlobSHA(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}
