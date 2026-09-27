// Package planner turns a task into a flow, and revises flows from chat.
// A model drafts YAML; the validator checks it; errors go back to the model
// until the flow is valid or attempts run out.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/resolve"
)

type Planner struct {
	cfg *config.Config
	llm *llm.Client
	gh  *github.Client
}

func New(cfg *config.Config, c *llm.Client, gh *github.Client) *Planner {
	return &Planner{cfg: cfg, llm: c, gh: gh}
}

type Request struct {
	FlowName string
	Project  string
	TaskRef  *flow.TaskRef
	Title    string
	Body     string
}

type Result struct {
	YAML        string
	Explanation string
	Issues      []resolve.Issue
	Attempts    int
	Valid       bool
}

// Turn is one earlier chat exchange, oldest first.
type Turn struct {
	Role    string // user | assistant
	Content string
}

// Plan drafts a new flow for a task.
func (p *Planner) Plan(ctx context.Context, req Request) (*Result, error) {
	proj := p.cfg.Projects[req.Project]
	if proj == nil {
		return nil, fmt.Errorf("unknown project %q", req.Project)
	}
	repoCtx := p.repoContext(ctx, proj)
	user := fmt.Sprintf("Design a flow for this task.\n\nFlow name: %s\nProject: %s\n\n## Task: %s\n\n%s\n\n%s",
		req.FlowName, req.Project, req.Title, strings.TrimSpace(req.Body), repoCtx)
	msgs := []llm.Message{
		{Role: "system", Content: p.systemPrompt(proj)},
		{Role: "user", Content: user},
	}
	return p.converge(ctx, msgs, req)
}

// Revise applies a chat instruction to an existing flow.
func (p *Planner) Revise(ctx context.Context, req Request, current string, history []Turn, message string) (*Result, error) {
	proj := p.cfg.Projects[req.Project]
	if proj == nil {
		return nil, fmt.Errorf("unknown project %q", req.Project)
	}
	msgs := []llm.Message{{Role: "system", Content: p.systemPrompt(proj)}}
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	for _, t := range history {
		msgs = append(msgs, llm.Message{Role: t.Role, Content: t.Content})
	}
	issues := p.validate(current, req)
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Task: %s\n\n%s\n\n", req.Title, strings.TrimSpace(req.Body))
	fmt.Fprintf(&sb, "## Current flow\n\n```yaml\n%s\n```\n\n", strings.TrimSpace(current))
	if len(issues) > 0 {
		sb.WriteString("## Current validation findings\n\n")
		for _, i := range issues {
			fmt.Fprintf(&sb, "- %s\n", i)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "## Change requested\n\n%s\n\nApply the change and reply with a short explanation and the complete updated flow in one ```yaml block.", message)
	msgs = append(msgs, llm.Message{Role: "user", Content: sb.String()})
	return p.converge(ctx, msgs, req)
}

func (p *Planner) converge(ctx context.Context, msgs []llm.Message, req Request) (*Result, error) {
	model := p.cfg.Catalog.Planner.Model
	if model == "" {
		return nil, fmt.Errorf("catalog.planner.model is not set")
	}
	res := &Result{}
	for attempt := 1; attempt <= p.cfg.Catalog.Planner.MaxAttempts; attempt++ {
		res.Attempts = attempt
		reply, usage, err := p.llm.Chat(ctx, model, msgs, llm.Options{Temperature: 0.2, MaxTokens: 8000})
		if err != nil {
			return nil, err
		}
		slog.Info("planner reply", "flow", req.FlowName, "attempt", attempt, "tokens", usage.PromptTokens+usage.CompletionTokens)
		explanation, body := splitReply(reply)
		if body == "" {
			msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
				llm.Message{Role: "user", Content: "I could not find a ```yaml block. Reply with the complete flow in one ```yaml block."})
			continue
		}
		fixed, perr := p.normalize(body, req)
		if perr != nil {
			msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
				llm.Message{Role: "user", Content: fmt.Sprintf("That YAML does not parse: %v\nReply with the complete corrected flow in one ```yaml block.", perr)})
			res.YAML, res.Explanation = body, explanation
			continue
		}
		res.YAML, res.Explanation = fixed, explanation
		res.Issues = p.validate(fixed, req)
		if !resolve.HasErrors(res.Issues) {
			res.Valid = true
			return res, nil
		}
		var sb strings.Builder
		sb.WriteString("The flow has validation errors. Fix all of them and reply with the complete corrected flow in one ```yaml block.\n\n")
		for _, i := range res.Issues {
			if i.Severity == resolve.Error {
				fmt.Fprintf(&sb, "- %s\n", i)
			}
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: reply}, llm.Message{Role: "user", Content: sb.String()})
	}
	return res, nil
}

func (p *Planner) validate(src string, req Request) []resolve.Issue {
	f, err := flow.Parse([]byte(src))
	if err != nil {
		return []resolve.Issue{{Severity: resolve.Error, Message: err.Error()}}
	}
	return resolve.Validate(resolve.Resolve(f, p.cfg), p.cfg)
}

// normalize parses the model's YAML, pins identity fields, and re-renders it
// in a stable layout.
func (p *Planner) normalize(src string, req Request) (string, error) {
	f, err := flow.Parse([]byte(src))
	if err != nil {
		return "", err
	}
	f.APIVersion, f.Kind = flow.APIVersion, flow.Kind
	f.Metadata.Name, f.Metadata.Project = req.FlowName, req.Project
	if req.TaskRef != nil {
		f.Metadata.Task = req.TaskRef
	}
	f.Metadata.CreatedBy = "planner/" + p.cfg.Catalog.Planner.Model
	return Render(f)
}

// Render writes a flow as YAML with nodes in flow order and fields in a readable order.
func Render(f *flow.Flow) (string, error) {
	var sb strings.Builder
	head := map[string]any{"apiVersion": f.APIVersion, "kind": f.Kind, "metadata": f.Metadata}
	hb, err := yaml.Marshal(head)
	if err != nil {
		return "", err
	}
	// yaml.Marshal sorts keys; put apiVersion and kind first by hand.
	fmt.Fprintf(&sb, "apiVersion: %s\nkind: %s\n", f.APIVersion, f.Kind)
	for _, line := range strings.Split(strings.TrimSpace(string(hb)), "\n") {
		if strings.HasPrefix(line, "apiVersion:") || strings.HasPrefix(line, "kind:") {
			continue
		}
		sb.WriteString(line + "\n")
	}
	sb.WriteString("spec:\n")
	spec := f.Spec
	if spec.Description != "" {
		writeField(&sb, "  ", "description", spec.Description)
	}
	for _, kv := range []struct {
		k string
		v any
	}{{"repo", spec.Repo}, {"budget", spec.Budget}, {"defaults", spec.Defaults}} {
		if isNil(kv.v) {
			continue
		}
		b, _ := yaml.Marshal(map[string]any{kv.k: kv.v})
		sb.WriteString(indent(string(b), "  "))
	}
	fmt.Fprintf(&sb, "  start: %s\n  nodes:\n", spec.Start)
	for i, id := range f.NodeIDs() {
		n := f.Spec.Nodes[id]
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "    %s:\n", id)
		sb.WriteString(renderNode(n, "      "))
	}
	return sb.String(), nil
}

var nodeFieldOrder = []string{
	"type", "uses", "description", "model", "llm", "harness", "runtime", "grants", "skills", "inputs",
	"prompt", "run", "exit_codes", "cases", "default", "action", "with", "outputs", "outcomes", "next",
	"max_visits", "on_exhausted", "timeout", "retry",
}

func renderNode(n *flow.Node, ind string) string {
	b, _ := yaml.Marshal(n)
	var m map[string]any
	yaml.Unmarshal(b, &m)
	var sb strings.Builder
	seen := map[string]bool{}
	emit := func(k string) {
		v, ok := m[k]
		if !ok || isEmpty(v) {
			return
		}
		seen[k] = true
		if s, ok := v.(string); ok && k == "prompt" || (ok && strings.Contains(s, "\n")) {
			writeField(&sb, ind, k, s)
			return
		}
		out, _ := yaml.Marshal(map[string]any{k: v})
		sb.WriteString(indent(flowStyle(k, string(out)), ind))
	}
	for _, k := range nodeFieldOrder {
		emit(k)
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		emit(k)
	}
	return sb.String()
}

// flowStyle keeps short lists and maps on one line (outcomes, next, grants).
func flowStyle(k, block string) string {
	switch k {
	case "outcomes", "grants", "skills", "next", "outputs", "exit_codes", "retry":
	default:
		return block
	}
	var v any
	if yaml.Unmarshal([]byte(block), &v) != nil {
		return block
	}
	inner := v.(map[string]any)[k]
	s := inline(inner)
	if len(s) > 100 {
		return block
	}
	return k + ": " + s + "\n"
}

var plainRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_./:$-]*$`)

func inline(v any) string {
	switch t := v.(type) {
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = inline(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + inline(t[k])
		}
		return "{" + strings.Join(parts, ", ") + "}" // matches the UI's YAML writer
	case string:
		if plainRe.MatchString(t) && t != "true" && t != "false" && t != "null" {
			return t
		}
		return fmt.Sprintf("%q", t)
	default:
		return fmt.Sprint(t)
	}
}

func writeField(sb *strings.Builder, ind, k, s string) {
	if !strings.Contains(s, "\n") && len(s) < 90 && !strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") {
		fmt.Fprintf(sb, "%s%s: %s\n", ind, k, s)
		return
	}
	indicator := "|-" // keep the value exactly: no trailing newline added
	if strings.HasSuffix(s, "\n") {
		indicator = "|"
	}
	if strings.HasPrefix(s, " ") || strings.HasSuffix(strings.TrimRight(s, "\n"), " ") || strings.HasSuffix(s, "\n\n") {
		b, _ := json.Marshal(s) // leading/trailing spaces don't survive block style
		fmt.Fprintf(sb, "%s%s: %s\n", ind, k, b)
		return
	}
	fmt.Fprintf(sb, "%s%s: %s\n", ind, k, indicator)
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString(ind + "  " + line + "\n")
	}
}

func indent(s, ind string) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		sb.WriteString(ind + line + "\n")
	}
	return sb.String()
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	case float64:
		return t == 0
	}
	return false
}

func isNil(v any) bool {
	switch t := v.(type) {
	case *flow.Repo:
		return t == nil
	case *flow.Budget:
		return t == nil
	case *flow.Node:
		return t == nil
	}
	return v == nil
}

var fenceRe = regexp.MustCompile("(?s)```(?:yaml|yml)?\\s*\\n(.*?)```")

// splitReply separates the explanation from the YAML block.
func splitReply(reply string) (string, string) {
	m := fenceRe.FindStringSubmatchIndex(reply)
	if m == nil {
		t := strings.TrimSpace(reply)
		if strings.HasPrefix(t, "apiVersion:") || strings.HasPrefix(t, "kind:") {
			return "", t
		}
		return strings.TrimSpace(reply), ""
	}
	body := reply[m[2]:m[3]]
	var parts []string
	for _, p := range []string{reply[:m[0]], reply[m[1]:]} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n"), body
}

// ---- prompt ----

func (p *Planner) repoContext(ctx context.Context, proj *config.Project) string {
	g := p.cfg.Catalog.Grants[proj.Spec.Repo]
	if g == nil || p.gh == nil {
		return ""
	}
	repo, err := github.ParseRepoURL(g.URL)
	if err != nil || repo.Host != "github.com" {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Repository %s (branch %s)\n\n", repo, proj.Spec.Base)
	paths, err := p.gh.Tree(ctx, repo, proj.Spec.Base)
	if err != nil {
		slog.Warn("planner: repo tree", "err", err)
		return ""
	}
	if len(paths) > 300 {
		fmt.Fprintf(&sb, "%d files; first 300:\n", len(paths))
		paths = paths[:300]
	}
	sb.WriteString("```\n" + strings.Join(paths, "\n") + "\n```\n")
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "README.md"} {
		content, err := p.gh.File(ctx, repo, proj.Spec.Base, name)
		if err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		if len(content) > 4000 {
			content = content[:4000] + "\n… (truncated)"
		}
		fmt.Fprintf(&sb, "\n### %s\n\n%s\n", name, content)
	}
	return sb.String()
}

func (p *Planner) systemPrompt(proj *config.Project) string {
	cat := &p.cfg.Catalog
	var sb strings.Builder
	sb.WriteString(rules)
	sb.WriteString("\n# Menu\n\nUse only these names.\n\n## Models (for `model:` on llm and agent nodes)\n\n")
	for _, name := range sortedKeys(cat.Models) {
		if !proj.AllowsModel(name) {
			continue
		}
		m := cat.Models[name]
		fmt.Fprintf(&sb, "- `%s`: size %s, context %d tokens, tool use %s, cost %s. %s\n", name, orDash(m.Size), m.ContextTokens, orDash(m.ToolUse), orDash(m.Cost), m.Notes)
	}
	sb.WriteString("\n## Runtimes (container images, `runtime:`; the default is fine for most nodes)\n\n")
	for _, name := range sortedKeys(cat.Runtimes) {
		fmt.Fprintf(&sb, "- `%s`: %s\n", name, cat.Runtimes[name].Description)
	}
	if cat.Defaults != nil && cat.Defaults.Runtime != "" {
		fmt.Fprintf(&sb, "Default runtime: `%s`.\n", cat.Defaults.Runtime)
	}
	sb.WriteString("\n## Grants (for `grants:`; pod nodes can always read the project repo)\n\n")
	for _, name := range sortedKeys(cat.Grants) {
		g := cat.Grants[name]
		if !proj.AllowsGrant(name) {
			continue
		}
		switch g.Kind {
		case config.GrantGit:
			fmt.Fprintf(&sb, "- `%s:write`: lets an agent's file changes be committed to the run branch. %s\n", name, g.Description)
		case config.GrantMCP:
			fmt.Fprintf(&sb, "- `%s`: MCP tools %s. %s\n", name, strings.Join(g.Tools, ", "), g.Description)
		default:
			fmt.Fprintf(&sb, "- `%s` (%s): %s\n", name, g.Kind, g.Description)
		}
	}
	if len(cat.Skills) > 0 {
		sb.WriteString("\n## Skills (for `skills:` on agent nodes)\n\n")
		for _, name := range sortedKeys(cat.Skills) {
			fmt.Fprintf(&sb, "- `%s`: %s\n", name, cat.Skills[name].Description)
		}
	}
	if len(cat.Presets) > 0 {
		sb.WriteString("\n## Presets (`uses: preset/<name>`; the preset supplies type, prompt, outputs and outcomes; you still set model, grants and next)\n\n")
		for _, name := range sortedKeys(cat.Presets) {
			pr := cat.Presets[name]
			var outs []string
			for k := range pr.Outputs {
				outs = append(outs, k)
			}
			sort.Strings(outs)
			fmt.Fprintf(&sb, "- `preset/%s` (%s, min model size %s): %s Outcomes: %s.", name, pr.Type, orDash(pr.MinSize), pr.Description, strings.Join(pr.Outcomes, ", "))
			if len(outs) > 0 {
				fmt.Fprintf(&sb, " Outputs: %s.", strings.Join(outs, ", "))
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n## Actions (`type: action`)\n\n- `open_pull_request`: opens a PR from the run branch. `with: { title, body, draft }`. Outcome: done.\n- `comment_task`: comments on the source task. `with: { body }`. Outcome: done.\n")
	fmt.Fprintf(&sb, "\n# Project `%s`\n\nRepo grant: `%s` (base branch `%s`). %s\n", proj.Metadata.Name, proj.Spec.Repo, proj.Spec.Base, proj.Spec.Description)
	sb.WriteString("\n# How much the human wants to be in the loop\n\n")
	if g := strings.TrimSpace(cat.Planner.Guidance); g != "" {
		sb.WriteString(g + "\n\n")
	}
	if g := strings.TrimSpace(proj.Spec.Planner.Guidance); g != "" {
		sb.WriteString(g + "\n\n")
	}
	sb.WriteString("When you add a `gate`, say why in its `description`. When you leave gates out of risky work, say why in the explanation.\n")
	sb.WriteString("\n# Example\n\n```yaml\n" + example + "```\n")
	return sb.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

const rules = `You design ai-flow flows. A flow is a small state machine that completes one task: each node is one narrow step that ends with exactly one outcome, and each outcome maps to the next node. Break the work down far enough that small local models can do each step.

# Flow format

` + "```yaml" + `
apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: <given>, project: <given> }
spec:
  description: <one line: the goal>
  start: <first node id>
  nodes:
    <node_id>:                 # lowercase_snake_case
      type: llm | agent | check | gate | switch | action   # omit when using a preset
      uses: preset/<name>      # optional
      description: <why this step exists>
      model: <model name>      # llm and agent nodes
      grants: [<grant>]        # agents that edit files need "<repo grant>:write"
      skills: [<skill>]        # agent nodes, optional
      prompt: |                # llm/agent: instructions for this step only. gate: the question for the human
      outputs: { <field>: string | number | bool | [string] }   # llm/agent, optional
      outcomes: [<a>, <b>]     # check nodes default to [pass, fail]
      next: { <a>: <node_id>, <b>: $success }   # map EVERY outcome; $success / $fail end the run
      max_visits: 3            # set on at least one node of every loop
      on_exhausted: <node_id or $fail>          # where to go when max_visits is used up
      run: <shell command>     # check nodes: exit 0 → pass, anything else → fail
      cases: [{ when: <CEL expression>, outcome: <x> }]   # switch nodes, plus default: <y>
      action: open_pull_request                  # action nodes, with: { title: ..., body: ... }
` + "```" + `

# Node types

- llm: one structured model call, no tools. Classify, review a diff (the branch diff is included automatically), extract, decide.
- agent: a coding agent in the repo with read/edit/write/bash. Use for changes and investigation. Give it one focused job.
- check: runs a shell command in the repo. Use for tests, linters, builds: anything deterministic.
- gate: waits for a human to pick an outcome. Only when the guidance below calls for it.
- switch: routes on a CEL expression over run.diff.files_changed, run.diff.lines_changed, nodes.<id>.outputs.<field>, nodes.<id>.outcome.
- action: open_pull_request or comment_task, run by ai-flow itself.

# Rules

- Every node needs outcomes and a next entry for each outcome.
- Loops are good (tests fail → fix again), but every loop needs max_visits on one of its nodes.
- Nodes automatically receive the task, a summary of earlier steps, and the previous step's outputs; do not repeat them in prompts. Use ${{ nodes.<id>.outputs.<field> }} or ${{ task.title }} only when a step needs a specific value.
- Prefer presets. Prefer the smallest model that can do the step.
- Keep prompts short and specific to the step.
- Reply with one or two sentences explaining the design, then exactly one ` + "```yaml" + ` block containing the complete flow.
`

const example = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: example-fix-date-parsing, project: example }
spec:
  description: Fix date parsing for ISO week dates.
  start: implement
  nodes:
    implement:
      uses: preset/implement
      model: qwen-local
      grants: [repo/example:write]
      max_visits: 3
      on_exhausted: $fail
      next: { done: test, stuck: $fail }
    test:
      type: check
      run: python -m unittest -v
      next: { pass: review, fail: implement }
    review:
      uses: preset/code-review
      model: gemma-local
      next: { approve: open_pr, changes: implement }
    open_pr:
      type: action
      action: open_pull_request
      with: { title: "${{ task.title }}" }
      outcomes: [done]
      next: { done: $success }
`
