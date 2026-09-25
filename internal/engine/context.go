package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/tmpl"
)

// TemplateContext is what ${{ }} references and switch CEL expressions see:
// task, run, nodes (latest visit of each node) and the node's own inputs.
func (e *Engine) TemplateContext(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node) map[string]any {
	return e.templateContext(ctx, r, res, n)
}

func (e *Engine) templateContext(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node) map[string]any {
	task := map[string]any{"title": res.Flow.Metadata.Name, "body": res.Flow.Spec.Description}
	if t := e.task(ctx, r); t != nil {
		task = map[string]any{"id": t.ID, "title": t.Title, "body": t.Body, "url": t.URL, "identifier": t.Identifier}
	}
	var diff map[string]any
	json.Unmarshal(r.Diff, &diff)
	if diff == nil {
		diff = map[string]any{}
	}
	for _, k := range []string{"files_changed", "lines_added", "lines_removed", "lines_changed"} {
		if _, ok := diff[k]; !ok {
			diff[k] = 0.0
		}
	}
	runCtx := map[string]any{"id": r.ID, "branch": r.Branch, "base": r.Base, "diff": diff, "pr_url": r.PRURL}
	nodes := map[string]any{}
	visits, _ := e.store.Visits(ctx, r.ID)
	var lastDone *store.Visit
	for _, v := range visits {
		if v.Status != store.VisitSucceeded {
			continue
		}
		var outputs map[string]any
		json.Unmarshal(v.Outputs, &outputs)
		nodes[v.Node] = map[string]any{"outcome": v.Outcome, "summary": v.Summary, "outputs": outputs, "visit": v.Visit}
		lastDone = v
	}
	if lastDone != nil {
		runCtx["last_node"] = lastDone.Node
		runCtx["last_outcome"] = lastDone.Outcome
	}
	c := map[string]any{"task": task, "run": runCtx, "nodes": nodes}
	inputs := map[string]any{}
	if n != nil {
		for k, s := range n.Inputs {
			v, _ := RenderText(s, c)
			inputs[k] = v
		}
	}
	c["inputs"] = inputs
	return c
}

// RenderText renders ${{ }} references.
func RenderText(s string, c map[string]any) (string, []string) {
	return tmpl.Render(s, c)
}

// ContextMarkdown is the context section a pod node gets: the task, the flow
// so far, and the step that routed here.
func (e *Engine) ContextMarkdown(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node, seq int) string {
	var sb strings.Builder
	title, body, ident := res.Flow.Metadata.Name, res.Flow.Spec.Description, ""
	if t := e.task(ctx, r); t != nil {
		title, body, ident = t.Title, t.Body, t.Identifier
	}
	sb.WriteString("## Task\n\n")
	if ident != "" {
		fmt.Fprintf(&sb, "**%s** (%s)\n\n", title, ident)
	} else {
		fmt.Fprintf(&sb, "**%s**\n\n", title)
	}
	if strings.TrimSpace(body) != "" {
		sb.WriteString(strings.TrimSpace(body))
		sb.WriteString("\n\n")
	}
	if res.Flow.Spec.Description != "" && res.Flow.Spec.Description != body {
		fmt.Fprintf(&sb, "Flow goal: %s\n\n", res.Flow.Spec.Description)
	}

	visits, _ := e.store.Visits(ctx, r.ID)
	var prev *store.Visit
	var done []*store.Visit
	for _, v := range visits {
		if v.Seq >= seq {
			break
		}
		if v.Status == store.VisitSucceeded {
			done = append(done, v)
			prev = v
		}
	}
	if len(done) > 0 {
		sb.WriteString("## Flow so far\n\n")
		start := 0
		if len(done) > 12 {
			start = len(done) - 12
			sb.WriteString("- …\n")
		}
		for _, v := range done[start:] {
			fmt.Fprintf(&sb, "- %s#%d → %s: %s\n", v.Node, v.Visit, v.Outcome, oneLine(v.Summary, 200))
		}
		sb.WriteString("\n")
	}
	if prev != nil {
		fmt.Fprintf(&sb, "## Previous step: %s → %s\n\n", prev.Node, prev.Outcome)
		if prev.Summary != "" {
			sb.WriteString(prev.Summary)
			sb.WriteString("\n\n")
		}
		var outputs map[string]any
		json.Unmarshal(prev.Outputs, &outputs)
		if logTail, ok := outputs["log_tail"].(string); ok && logTail != "" {
			delete(outputs, "log_tail")
			fmt.Fprintf(&sb, "Output (tail):\n\n```\n%s\n```\n\n", strings.TrimSpace(tailStr(logTail, 3000)))
		}
		if len(outputs) > 0 {
			b, _ := json.MarshalIndent(outputs, "", "  ")
			fmt.Fprintf(&sb, "Outputs:\n\n```json\n%s\n```\n\n", b)
		}
	}
	c := e.templateContext(ctx, r, res, n)
	if inputs, _ := c["inputs"].(map[string]any); len(inputs) > 0 {
		sb.WriteString("## Inputs\n\n")
		for k, v := range inputs {
			fmt.Fprintf(&sb, "- %s: %s\n", k, tmpl.Stringify(v))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "## Branch\n\nWorking on `%s` (base `%s`).", r.Branch, r.Base)
	var diff map[string]any
	json.Unmarshal(r.Diff, &diff)
	if f, _ := diff["files_changed"].(float64); f > 0 {
		fmt.Fprintf(&sb, " So far: %v files changed, +%v/-%v lines.", diff["files_changed"], diff["lines_added"], diff["lines_removed"])
	}
	sb.WriteString("\n")
	return sb.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (e *Engine) evalSwitch(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node) (string, string, error) {
	c := e.templateContext(ctx, r, res, n)
	for _, cs := range n.Cases {
		ok, err := resolve.EvalCEL(cs.When, c)
		if err != nil {
			return "", "", fmt.Errorf("case %q: %v", cs.When, err)
		}
		if ok {
			return cs.Outcome, fmt.Sprintf("`%s` is true", cs.When), nil
		}
	}
	return n.Default, "No case matched", nil
}

type actionResult struct {
	outcome, summary string
	outputs          map[string]any
}

func (e *Engine) runAction(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node) (*actionResult, error) {
	c := e.templateContext(ctx, r, res, n)
	with := map[string]string{}
	for k, v := range n.With {
		if s, ok := v.(string); ok {
			with[k], _ = RenderText(s, c)
		} else {
			with[k] = fmt.Sprint(v)
		}
	}
	switch n.Action {
	case "open_pull_request":
		return e.openPR(ctx, r, res, with)
	case "comment_task":
		t := e.task(ctx, r)
		if t == nil || e.hooks == nil {
			return &actionResult{outcome: "done", summary: "No linked task to comment on"}, nil
		}
		if c, ok := e.hooks.(interface {
			Comment(ctx context.Context, t *store.Task, body string) error
		}); ok {
			if err := c.Comment(ctx, t, with["body"]); err != nil {
				return nil, err
			}
		}
		return &actionResult{outcome: "done", summary: "Commented on " + t.Identifier}, nil
	}
	return nil, fmt.Errorf("unknown action %q", n.Action)
}

func (e *Engine) openPR(ctx context.Context, r *store.Run, res *resolve.Resolved, with map[string]string) (*actionResult, error) {
	if e.gh == nil {
		return nil, fmt.Errorf("github is not configured")
	}
	g := e.cfg.Catalog.Grants[res.Repo]
	if g == nil {
		return nil, fmt.Errorf("flow has no repo")
	}
	repo, err := github.ParseRepoURL(g.URL)
	if err != nil {
		return nil, err
	}
	exists, err := e.gh.BranchExists(ctx, repo, r.Branch)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("branch %s has no commits to open a pull request from", r.Branch)
	}
	title := with["title"]
	t := e.task(ctx, r)
	if title == "" {
		if t != nil {
			title = t.Title
		} else {
			title = res.Flow.Metadata.Name
		}
	}
	body := strings.TrimSpace(with["body"])
	if body == "" && t != nil {
		body = strings.TrimSpace(t.Body)
	}
	body += "\n\n" + e.prFooter(ctx, r, t)
	pr, err := e.gh.OpenPR(ctx, repo, r.Branch, r.Base, title, strings.TrimSpace(body), with["draft"] == "true")
	if err != nil {
		return nil, err
	}
	e.store.UpdateRun(ctx, r.ID, map[string]any{"pr_url": pr.HTMLURL})
	r.PRURL = pr.HTMLURL
	return &actionResult{outcome: "done", summary: "Opened " + pr.HTMLURL, outputs: map[string]any{"url": pr.HTMLURL, "number": pr.Number}}, nil
}

func (e *Engine) prFooter(ctx context.Context, r *store.Run, t *store.Task) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Made by [ai-flow](%s/runs/%s) · flow `%s` v%d", strings.TrimSuffix(e.cfg.Env.Server.PublicURL, "/"), r.ID, r.FlowName, r.FlowVersion)
	if t != nil && t.URL != "" {
		fmt.Fprintf(&sb, " · task [%s](%s)", firstNonEmpty(t.Identifier, t.Title), t.URL)
	}
	sb.WriteString("\n\n| Step | Outcome | Summary |\n|---|---|---|\n")
	visits, _ := e.store.Visits(ctx, r.ID)
	for _, v := range visits {
		if v.Status != store.VisitSucceeded {
			continue
		}
		fmt.Fprintf(&sb, "| %s#%d | %s | %s |\n", v.Node, v.Visit, v.Outcome, strings.ReplaceAll(oneLine(v.Summary, 160), "|", "\\|"))
	}
	return sb.String()
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
