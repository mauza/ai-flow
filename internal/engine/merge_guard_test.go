package engine

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		glob, file string
		want       bool
	}{
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", ".githubx/a", false},
		{"scripts/check_*.py", "scripts/check_migrations.py", true},
		{"scripts/check_*.py", "scripts/sub/check_a.py", false},
		{"Makefile", "web/Makefile", false},
	} {
		if matchPath(tc.glob, tc.file) != tc.want {
			t.Errorf("%s %s", tc.glob, tc.file)
		}
	}
}

func TestReviewMissing(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	write := []string{"repo/ai-flow-sandbox:write"}
	node := func(preset string, grants []string, outcomes ...string) *resolve.Node {
		n := &resolve.Node{Preset: preset}
		n.Type, n.Grants, n.Outcomes = flow.TypeAgent, grants, outcomes
		return n
	}
	res := &resolve.Resolved{Nodes: map[string]*resolve.Node{
		"work":     node("implement", write, "done"),
		"fix":      node("", write, "done"),
		"review":   node("code-review", nil, "approve", "changes"),
		"security": node("security-review", nil, "approve", "changes"),
		"homemade": node("", nil, "approve"),
		"update":   node("update-branch", write, "merged"),
	}}
	run := func(steps ...string) string { // "node model outcome"
		var vs []*store.Visit
		for i, s := range steps {
			f := strings.Fields(s)
			vs = append(vs, &store.Visit{Seq: i + 1, Node: f[0], Model: f[1], Outcome: f[2], Status: store.VisitSucceeded})
		}
		return reviewMissing(vs, res, cfg)
	}
	for _, tc := range []struct {
		name, want string
		steps      []string
	}{
		{"approved by another model", "", []string{"work sol done", "review luna approve"}},
		{"no review", "no review preset approved", []string{"work sol done"}},
		{"own model", "only the model that wrote", []string{"work sol done", "review sol approve"}},
		{"an author's model reviews a fix by another", "only the model that wrote", []string{"work luna done", "fix sol done", "review luna approve"}},
		{"changed after the review", "no review preset approved", []string{"work sol done", "review luna approve", "fix sol done"}},
		{"rejected", "returned changes", []string{"work sol done", "review luna approve", "security luna changes"}},
		{"approved again after the fix", "", []string{"work sol done", "review luna changes", "fix sol done", "review luna approve"}},
		{"another review's old objection was fixed", "", []string{"work sol done", "security luna changes", "fix sol done", "review luna approve"}},
		{"update from the base keeps the review", "", []string{"work sol done", "review luna approve", "update sol merged"}},
		{"a step that only calls itself a review", "no review preset approved", []string{"work sol done", "homemade luna approve"}},
	} {
		got := run(tc.steps...)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
