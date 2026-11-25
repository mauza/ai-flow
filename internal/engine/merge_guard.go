package engine

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// mergeRefused says why the project's own rules keep ai-flow from merging, or
// "". They hold whatever the flow says: a flow is written by a model.
func (e *Engine) mergeRefused(ctx context.Context, r *store.Run, res *resolve.Resolved, repo github.Repo, number int) (string, error) {
	p := res.Project
	if p == nil {
		return "", nil
	}
	if d := p.Spec.Deploy; d != nil && len(d.Protected) > 0 {
		files, err := e.gh.PRFiles(ctx, repo, number)
		if err != nil {
			return "", err
		}
		var hit []string
		for _, f := range files {
			if slices.ContainsFunc(d.Protected, func(g string) bool { return matchPath(g, f) }) {
				hit = append(hit, f)
			}
		}
		if len(hit) > 0 {
			return fmt.Sprintf("it changes files only a person merges (deploy.protected): %s", strings.Join(hit[:min(len(hit), 5)], ", ")), nil
		}
	}
	if rv := p.Spec.Review; rv != nil && rv.Required {
		vs, err := e.store.Visits(ctx, r.ID)
		if err != nil {
			return "", err
		}
		return reviewMissing(vs, res, e.cfg.Current()), nil
	}
	return "", nil
}

// matchPath matches a repository path against a glob; "dir/**" is everything
// under dir.
func matchPath(glob, file string) bool {
	if dir, ok := strings.CutSuffix(glob, "/**"); ok {
		return strings.HasPrefix(file, dir+"/")
	}
	ok, err := path.Match(glob, file)
	return err == nil && ok
}

// baseMerges are the presets whose commits only bring in, or undo, work that was
// reviewed when it was written.
var baseMerges = []string{"update-branch", "revert-deploy", "resolve-revert"}

// reviewMissing says what the run's reviews lack: after the last step that wrote
// code, a review on another model than any author's must have approved, and no
// review may have ended otherwise.
func reviewMissing(vs []*store.Visit, res *resolve.Resolved, cfg *config.Config) string {
	lastWrite := 0
	authors := map[string]bool{}
	for _, v := range vs {
		n := res.Nodes[v.Node]
		if n == nil || n.Type != flow.TypeAgent || v.Status != store.VisitSucceeded || slices.Contains(baseMerges, n.Preset) || !writesRepo(n, cfg) {
			continue
		}
		lastWrite = v.Seq
		authors[v.Model] = true
	}
	latest := map[string]*store.Visit{}
	for _, v := range vs {
		n := res.Nodes[v.Node]
		if n == nil || v.Seq <= lastWrite || v.Status != store.VisitSucceeded {
			continue
		}
		if p := cfg.Catalog.Presets[n.Preset]; p != nil && p.Category == "review" && slices.Contains(n.Outcomes, "approve") {
			latest[v.Node] = v
		}
	}
	independent := false
	for id, v := range latest {
		if v.Outcome != "approve" {
			return fmt.Sprintf("%s#%d returned %s on the final code", id, v.Visit, v.Outcome)
		}
		independent = independent || (v.Model != "" && !authors[v.Model])
	}
	if len(latest) == 0 {
		return "no review preset approved the code after it last changed (review.required)"
	}
	if !independent {
		return "only the model that wrote the code reviewed it (review.required)"
	}
	return ""
}

func writesRepo(n *resolve.Node, cfg *config.Config) bool {
	for _, g := range n.Grants {
		name, mode, _ := strings.Cut(g, ":")
		if grant := cfg.Catalog.Grants[name]; grant != nil && grant.Kind == config.GrantGit && mode == "write" {
			return true
		}
	}
	return false
}
