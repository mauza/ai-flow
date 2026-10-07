package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	c, err := e.checkedTemplateContext(ctx, r, res, n)
	if err != nil {
		slog.Error("template context", "run", r.ID, "err", err)
	}
	return c
}

func (e *Engine) checkedTemplateContext(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node) (map[string]any, error) {
	task := map[string]any{"title": res.Flow.Metadata.Name, "body": res.Flow.Spec.Description}
	t, err := e.optionalTask(ctx, r)
	if err != nil {
		return nil, err
	}
	if t != nil {
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
	runCtx := map[string]any{"id": r.ID, "branch": r.Branch, "base": r.Base, "diff": diff, "pr_url": r.PRURL, "resumes": r.Resumes, "resume_note": r.ResumeNote}
	nodes := map[string]any{}
	visits, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	var lastDone *store.Visit
	history := map[string][]any{}
	for _, v := range visits {
		if v.Status != store.VisitSucceeded {
			continue
		}
		var outputs map[string]any
		json.Unmarshal(v.Outputs, &outputs)
		history[v.Node] = append(history[v.Node], map[string]any{"visit": v.Visit, "outcome": v.Outcome, "summary": v.Summary, "outputs": outputs})
		nodes[v.Node] = map[string]any{"outcome": v.Outcome, "summary": v.Summary, "outputs": outputs, "visit": v.Visit}
		lastDone = v
	}
	// history: every successful visit of the node, oldest first. Loops use it
	// to see earlier passes (e.g. all review comments, not just the latest).
	for id, h := range history {
		nodes[id].(map[string]any)["history"] = h
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
	return c, nil
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
		writeLoopHistory(&sb, done)
	}
	if r.Resumes > 0 && seq > r.ResumeSeq {
		writeResume(&sb, r, visits)
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

const loopHistoryBudget = 8000

// writeLoopHistory lists every pass of each step that ran more than once, with
// its outputs, so a step in a loop can see all earlier feedback rather than only
// the latest. When it is too long, the oldest passes are dropped first.
func writeLoopHistory(sb *strings.Builder, done []*store.Visit) {
	count := map[string]int{}
	for _, v := range done {
		count[v.Node]++
	}
	var lines []string
	for _, v := range done {
		if count[v.Node] < 2 {
			continue
		}
		line := fmt.Sprintf("- %s#%d → %s: %s", v.Node, v.Visit, v.Outcome, oneLine(v.Summary, 300))
		var outputs map[string]any
		json.Unmarshal(v.Outputs, &outputs)
		delete(outputs, "log_tail")
		if len(outputs) > 0 {
			b, _ := json.Marshal(outputs)
			line += "\n  outputs: " + oneLine(string(b), 800)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	trimmed := false
	for size(lines) > loopHistoryBudget && len(lines) > 1 {
		lines, trimmed = lines[1:], true
	}
	sb.WriteString("## Loop history\n\nEvery pass of the steps that repeated, oldest first. Do not undo fixes made for earlier feedback.\n\n")
	if trimmed {
		sb.WriteString("- … earlier passes trimmed\n")
	}
	sb.WriteString(strings.Join(lines, "\n"))
	sb.WriteString("\n\n")
}

func size(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

// writeResume tells steps after a resume why the run stopped and what the
// human who resumed it said.
func writeResume(sb *strings.Builder, r *store.Run, visits []*store.Visit) {
	sb.WriteString("## Resumed\n\nThis run stopped")
	for i := len(visits) - 1; i >= 0; i-- {
		v := visits[i]
		if v.Seq > r.ResumeSeq {
			continue
		}
		if v.Status == store.VisitError || v.Status == store.VisitCanceled {
			fmt.Fprintf(sb, " at %s#%d (%s)", v.Node, v.Visit, oneLine(v.Error, 300))
		} else {
			fmt.Fprintf(sb, " after %s#%d → %s", v.Node, v.Visit, v.Outcome)
		}
		break
	}
	sb.WriteString(" and a human resumed it.")
	if r.ResumeNote != "" {
		fmt.Fprintf(sb, " Their note:\n\n> %s", strings.ReplaceAll(strings.TrimSpace(r.ResumeNote), "\n", "\n> "))
	}
	sb.WriteString("\n\n")
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
	c, err := e.checkedTemplateContext(ctx, r, res, n)
	if err != nil {
		return "", "", err
	}
	for _, cs := range n.Cases {
		ok, err := resolve.EvalCEL(cs.When, c)
		if err != nil {
			return "", "", rejectf("case %q: %v", cs.When, err)
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

func (e *Engine) runAction(ctx context.Context, r *store.Run, res *resolve.Resolved, n *resolve.Node, v *store.Visit) (*actionResult, error) {
	c, err := e.checkedTemplateContext(ctx, r, res, n)
	if err != nil {
		return nil, err
	}
	t, err := e.optionalTask(ctx, r)
	if err != nil {
		return nil, err
	}
	with := map[string]string{}
	for k, v := range n.With {
		if s, ok := v.(string); ok {
			with[k], _ = RenderText(s, c)
		} else {
			with[k] = fmt.Sprint(v)
		}
	}
	// Read dependencies before marking delivery started. A transient database
	// read failure is retryable and must not make a comment look delivered.
	fields := map[string]any{"status": store.VisitRunning}
	if v.StartedAt == 0 {
		fields["started_at"] = store.Now()
	}
	if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, fields); err != nil {
		return nil, err
	}
	e.publishVisit(r.ID, v.Seq)
	switch n.Action {
	case "open_pull_request":
		return e.openPR(ctx, r, res, t, with)
	case "comment_task":
		if t == nil || e.hooks == nil {
			return &actionResult{outcome: "done", summary: "No linked task to comment on"}, nil
		}
		if c, ok := e.hooks.(interface {
			Comment(ctx context.Context, t *store.Task, body string) error
		}); ok {
			if err := c.Comment(ctx, t, with["body"]); err != nil {
				return nil, rejectf("comment delivery uncertain: %v; inspect the task before retrying manually", err)
			}
		}
		return &actionResult{outcome: "done", summary: "Commented on " + t.Identifier}, nil
	}
	return nil, rejectf("unknown action %q", n.Action)
}

func (e *Engine) openPR(ctx context.Context, r *store.Run, res *resolve.Resolved, t *store.Task, with map[string]string) (*actionResult, error) {
	if e.gh == nil {
		return nil, rejectf("github is not configured")
	}
	g := e.cfg.Catalog.Grants[res.Repo]
	if g == nil {
		return nil, rejectf("flow has no repo")
	}
	repo, err := github.ParseRepoURL(g.URL)
	if err != nil {
		return nil, rejection{err}
	}
	exists, err := e.gh.BranchExists(ctx, repo, r.Branch)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, rejectf("branch %s has no commits to open a pull request from", r.Branch)
	}
	title := with["title"]
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
	footer, err := e.prFooter(ctx, r, t)
	if err != nil {
		return nil, err
	}
	body += "\n\n" + footer
	pr, err := e.gh.OpenPR(ctx, repo, r.Branch, r.Base, title, strings.TrimSpace(body), with["draft"] == "true")
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdateRun(ctx, r.ID, map[string]any{"pr_url": pr.HTMLURL}); err != nil {
		return nil, err
	}
	r.PRURL = pr.HTMLURL
	return &actionResult{outcome: "done", summary: "Opened " + pr.HTMLURL, outputs: map[string]any{"url": pr.HTMLURL, "number": pr.Number}}, nil
}

func (e *Engine) prFooter(ctx context.Context, r *store.Run, t *store.Task) (string, error) {
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Made by [ai-flow](%s/runs/%s) · flow `%s` v%d", strings.TrimSuffix(e.cfg.Env.Server.PublicURL, "/"), r.ID, r.FlowName, r.FlowVersion)
	if t != nil && t.URL != "" {
		fmt.Fprintf(&sb, " · task [%s](%s)", firstNonEmpty(t.Identifier, t.Title), t.URL)
	}
	sb.WriteString("\n\n| Step | Outcome | Summary |\n|---|---|---|\n")
	visits, err := e.store.Visits(ctx, r.ID)
	if err != nil {
		return "", err
	}
	for _, v := range visits {
		if v.Status != store.VisitSucceeded {
			continue
		}
		fmt.Fprintf(&sb, "| %s#%d | %s | %s |\n", v.Node, v.Visit, v.Outcome, strings.ReplaceAll(oneLine(v.Summary, 160), "|", "\\|"))
	}
	return sb.String(), nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
