package workspace_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/github/githubtest"
	"github.com/mauza/ai-flow/internal/workspace"
)

func setup(t *testing.T, files map[string]string) (*workspace.Manager, *githubtest.Repo, workspace.Source) {
	repo := githubtest.New("o", "app", files)
	srv := httptest.NewServer(repo)
	t.Cleanup(srv.Close)
	m := workspace.New(t.TempDir(), github.New(srv.URL, ""), func() int64 { return 1 })
	return m, repo, workspace.Source{Repo: github.Repo{Host: "github.com", Owner: "o", Name: "app"}, Branch: "main"}
}

func paths(cs []workspace.Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Kind+" "+c.Path)
	}
	sort.Strings(out)
	return out
}

func TestCheckoutEditCommit(t *testing.T) {
	ctx := context.Background()
	m, repo, src := setup(t, map[string]string{
		"product/README.md": "# App\n",
		"product/old.md":    "old\n",
		"src/main.go":       "package main\n",
	})
	st, err := m.Ensure(ctx, "app", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Changes) != 0 {
		t.Fatalf("fresh checkout has changes: %v", st.Changes)
	}
	if b, err := m.Read("app", "product/README.md"); err != nil || string(b) != "# App\n" {
		t.Fatalf("read: %q %v", b, err)
	}
	if _, err := m.Read("app", "src/main.go"); err == nil {
		t.Fatal("only product/ is checked out and readable")
	}

	m.Write("app", "product/README.md", []byte("# App v2\n"))
	m.Write("app", "product/user-story-maps/arcade/map.yaml", []byte("title: Arcade\n"))
	m.Remove("app", "product/old.md")
	st, _ = m.Status("app")
	if got := strings.Join(paths(st.Changes), ","); got != "added product/user-story-maps/arcade/map.yaml,deleted product/old.md,modified product/README.md" {
		t.Fatalf("changes: %s", got)
	}

	sha, err := m.Commit(ctx, "app", src, "Product: arcade map", map[string]string{"name": "ai-flow", "email": "a@b"})
	if err != nil {
		t.Fatal(err)
	}
	files := repo.Files()
	if repo.Head() != sha || files["product/README.md"] != "# App v2\n" || files["product/user-story-maps/arcade/map.yaml"] != "title: Arcade\n" {
		t.Fatalf("commit did not land: %v", files)
	}
	if _, ok := files["product/old.md"]; ok {
		t.Fatal("deleted file is still in the repo")
	}
	if files["src/main.go"] != "package main\n" {
		t.Fatal("files outside product/ must be untouched")
	}
	if st, _ = m.Status("app"); len(st.Changes) != 0 || st.Head != sha {
		t.Fatalf("after commit: %+v", st)
	}
	if _, err := m.Commit(ctx, "app", src, "again", nil); err == nil {
		t.Fatal("an empty commit must fail")
	}
}

func TestCommitRefusesWhenTheBranchMoved(t *testing.T) {
	ctx := context.Background()
	m, repo, src := setup(t, map[string]string{"product/README.md": "a\n", "product/b.md": "b\n"})
	m.Ensure(ctx, "app", src)
	m.Write("app", "product/README.md", []byte("mine\n"))
	repo.Push(map[string]string{"product/b.md": "theirs\n"})

	if _, err := m.Commit(ctx, "app", src, "edit", nil); !errors.Is(err, github.ErrNotFastForward) {
		t.Fatalf("got %v, want not fast forward", err)
	}
	// Pulling brings their change and keeps my edit; then the commit works.
	res, err := m.Pull(ctx, "app", src)
	if err != nil || len(res.Updated) != 1 {
		t.Fatalf("pull: %+v %v", res, err)
	}
	if b, _ := m.Read("app", "product/README.md"); string(b) != "mine\n" {
		t.Fatal("pull lost a local edit")
	}
	if b, _ := m.Read("app", "product/b.md"); string(b) != "theirs\n" {
		t.Fatal("pull did not bring the remote change")
	}
	if _, err := m.Commit(ctx, "app", src, "edit", nil); err != nil {
		t.Fatal(err)
	}
	if f := repo.Files(); f["product/README.md"] != "mine\n" || f["product/b.md"] != "theirs\n" {
		t.Fatalf("final: %v", f)
	}
}

func TestPullRefusesToOverwriteLocalEdits(t *testing.T) {
	ctx := context.Background()
	m, repo, src := setup(t, map[string]string{"product/README.md": "a\n"})
	m.Ensure(ctx, "app", src)
	m.Write("app", "product/README.md", []byte("mine\n"))
	repo.Push(map[string]string{"product/README.md": "theirs\n"})

	res, err := m.Pull(ctx, "app", src)
	if !errors.Is(err, workspace.ErrConflict) || len(res.Conflicts) != 1 {
		t.Fatalf("got %+v %v", res, err)
	}
	if b, _ := m.Read("app", "product/README.md"); string(b) != "mine\n" {
		t.Fatal("a refused pull must not touch files")
	}
	// Discarding the edit resolves it.
	if err := m.Discard("app", []string{"product/README.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pull(ctx, "app", src); err != nil {
		t.Fatal(err)
	}
	if b, _ := m.Read("app", "product/README.md"); string(b) != "theirs\n" {
		t.Fatalf("after discard and pull: %q", b)
	}
}

func TestPathsStayInsideProduct(t *testing.T) {
	m, _, _ := setup(t, map[string]string{})
	for _, p := range []string{"src/x.go", "product/../x", "/product/x", "product/./x", "product/.git/config", "product"} {
		if err := m.Write("app", p, []byte("x")); err == nil {
			t.Errorf("%s: write allowed", p)
		}
	}
}
