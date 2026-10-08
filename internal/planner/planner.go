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
	if p.cfg.Catalog.Planner.MaxAttempts < 1 {
		return nil, fmt.Errorf("catalog.planner.max_attempts must be positive")
	}
	res := &Result{}
	firstExplanation := ""
	for attempt := 1; attempt <= p.cfg.Catalog.Planner.MaxAttempts; attempt++ {
		res.Attempts = attempt
		reply, usage, err := p.llm.Chat(ctx, model, msgs, llm.Options{Temperature: 0.2, MaxTokens: 8000, Stream: p.cfg.Catalog.Planner.Stream})
		if err != nil {
			return nil, err
		}
		slog.Info("planner reply", "flow", req.FlowName, "attempt", attempt, "tokens", usage.PromptTokens+usage.CompletionTokens)
		explanation, body := splitReply(reply)
		// A repair reply is often just the corrected YAML; keep the design note.
		if strings.TrimSpace(explanation) == "" {
			explanation = firstExplanation
		} else if firstExplanation == "" {
			firstExplanation = explanation
		}
		// Every result field describes this attempt, including failed extraction
		// and parsing. Never pair a new candidate with an earlier diagnostic.
		res = &Result{Attempts: attempt, YAML: body, Explanation: explanation}
		if strings.TrimSpace(body) == "" {
			res.YAML = ""
			res.Issues = []resolve.Issue{{Severity: resolve.Error, Message: "planner reply did not contain a non-empty YAML flow"}}
			msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
				llm.Message{Role: "user", Content: "I could not find a ```yaml block. Reply with the complete flow in one ```yaml block."})
			continue
		}
		fixed, perr := p.normalize(body, req)
		if perr != nil {
			res.Issues = []resolve.Issue{{Severity: resolve.Error, Message: perr.Error()}}
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
		var errs []string
		for _, i := range res.Issues {
			if i.Severity == resolve.Error {
				fmt.Fprintf(&sb, "- %s\n", i)
				errs = append(errs, i.String())
			}
		}
		slog.Info("planner repair", "flow", req.FlowName, "attempt", attempt, "errors", strings.Join(errs, " | "))
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
	menu := ProjectEligibility(p.cfg, proj)
	for _, name := range menu.AllowedModels {
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
	for _, entry := range menu.AllowedGrants {
		name, mode, _ := strings.Cut(entry, ":")
		g := cat.Grants[name]
		switch g.Kind {
		case config.GrantGit:
			if mode == "" {
				mode = "read"
			}
			fmt.Fprintf(&sb, "- `%s`: git %s access. %s\n", entry, mode, g.Description)
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
		writePresetMenu(&sb, cat.Presets)
	}
	sb.WriteString("\n## Actions (`type: action`; route every outcome)\n\n")
	for _, name := range flow.ActionNames() {
		spec := flow.Actions[name]
		fmt.Fprintf(&sb, "- `%s`: %s", name, spec.Doc)
		if len(spec.With) > 0 {
			fmt.Fprintf(&sb, " with: %s.", strings.Join(spec.With, ", "))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\n# Project `%s`\n\nRepo grant: `%s` (base branch `%s`). %s\n", proj.Metadata.Name, proj.Spec.Repo, proj.Spec.Base, proj.Spec.Description)
	if d := proj.Spec.Deploy; d != nil {
		writeDeploy(&sb, proj.Spec.Base, d)
	}
	sb.WriteString("\n# Planning guidance\n\n")
	if g := strings.TrimSpace(cat.Planner.Guidance); g != "" {
		sb.WriteString(g + "\n\n")
	}
	if g := strings.TrimSpace(proj.Spec.Planner.Guidance); g != "" {
		sb.WriteString(g + "\n\n")
	}
	sb.WriteString("Gates are optional: follow task/project guidance. When you add a `gate`, describe the specific decision for the human.\n")
	sb.WriteString("\n# Example\n\n```yaml\n" + example + "```\n")
	return sb.String()
}

// Keep model instructions in the preset itself. The planner needs selection
// cues and wiring contracts, including executable fields for non-model nodes.
func writePresetMenu(sb *strings.Builder, presets map[string]*config.Preset) {
	sb.WriteString("\n## Presets (`uses: preset/<name>`; inherit the definition, set model for llm/agent, write grants for edits, and next for EVERY outcome)\n\n")
	names := sortedKeys(presets)
	sort.SliceStable(names, func(i, j int) bool { return presets[names[i]].Category < presets[names[j]].Category })
	category := ""
	for _, name := range names {
		pr := presets[name]
		if group := orDash(pr.Category); group != category {
			category = group
			fmt.Fprintf(sb, "### %s\n\n", category)
		}
		fmt.Fprintf(sb, "- `preset/%s` (%s", name, pr.Type)
		if pr.MinSize != "" {
			fmt.Fprintf(sb, ", min model %s", pr.MinSize)
		}
		fmt.Fprintf(sb, "): %s", pr.Description)
		if pr.WhenToUse != "" {
			fmt.Fprintf(sb, " Use when: %s", pr.WhenToUse)
		}
		if len(pr.Requires) > 0 {
			fmt.Fprintf(sb, " Requires: %s.", strings.Join(pr.Requires, "; "))
		}
		fmt.Fprintf(sb, " Outcomes: %s.", strings.Join(pr.Outcomes, ", "))
		if len(pr.Outputs) > 0 {
			fmt.Fprintf(sb, " Outputs: %s.", inline(pr.Outputs))
		}
		// JSON keeps multiline commands on one line and preserves value types.
		wiring := map[string]any{}
		if pr.Runtime != "" {
			wiring["runtime"] = pr.Runtime
		}
		if pr.MaxVisits > 0 {
			wiring["max_visits"] = pr.MaxVisits
			wiring["on_exhausted"] = pr.OnExhausted
		}
		switch pr.Type {
		case flow.TypeCheck:
			if pr.Run == "" {
				sb.WriteString(" Configure run: REQUIRED (validation rejects an empty command).")
			} else {
				wiring["run"] = pr.Run
			}
			wiring["exit_codes"] = resolve.CheckExitCodes(&pr.Node)
		case flow.TypeSwitch:
			wiring["cases"], wiring["default"] = pr.Cases, pr.Default
		case flow.TypeAction:
			wiring["action"], wiring["with"] = pr.Action, pr.With
		case flow.TypeGate:
			wiring["prompt"] = pr.Prompt
			if pr.Timeout.Duration > 0 {
				wiring["timeout"] = pr.Timeout
			}
		}
		if len(pr.Inputs) > 0 {
			wiring["inputs"] = pr.Inputs
		}
		if len(wiring) > 0 {
			b, _ := json.Marshal(wiring)
			fmt.Fprintf(sb, " Defaults: %s", b)
		}
		sb.WriteString("\n")
	}
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

const rules = `You design ai-flow flows. A flow is a small state machine that completes one task: each node is one narrow step that ends with exactly one outcome, and each outcome maps to the next node. Break the work down into focused, verifiable steps using the smallest sufficient configured model.

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
      type: llm | agent | check | gate | switch | action | parallel | join   # omit when using a preset
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
      branches: [<node_id>, ...]                # parallel nodes: the first node of each branch
      join: <join node id>                       # parallel nodes: where every branch ends
` + "```" + `

# Node types

- llm: one structured model call, no tools. Classify, review a diff (the branch diff is included automatically), extract, decide.
- agent: a coding agent in the repo with read/edit/write/bash. Use for changes and investigation. Give it one focused job.
- check: runs bash -o pipefail -c in a fresh repo checkout, with CI=true. No implicit errexit: use && or set -e for multiple commands. Use for tests, linters, builds. Outputs are exit_code (integer), log_tail (string). For structured results (counts, coverage, failing names), declare outputs and have the command write one JSON object with those fields to "$AI_FLOW_OUTPUTS"; a switch can then route on them. Undeclared or mistyped fields fail the step.
- gate: waits for a human to pick an outcome. Only when the guidance below calls for it.
- switch: routes on a CEL expression over run.diff.files_changed, run.diff.lines_changed, nodes.<id>.outputs.<field>, nodes.<id>.outcome.
- action: open_pull_request or comment_task, run by ai-flow itself.
- parallel: starts all its branches at once. Each branch is a path of nodes that begins at a listed node and ends by routing to the parallel node's join. Branches are read-only (no repo write grants), may contain only llm, agent, check and switch nodes, may route to $fail but not $success, and nothing outside a branch may route into it. A parallel node has no outcomes or next of its own.
- join: waits until every branch has reached it, then routes like a switch: cases over nodes.<id>.outcome / nodes.<id>.outputs from any branch, plus default. Without cases its single outcome is done.

# Choosing rigor

Match rigor to how a wrong plan would fail, not to how important the task sounds:

- Light: implement → check → open a PR. For small, reversible, well-understood changes.
- Test-first (the default for behavior changes): a failing test → implement → check → review.
- Investigate first: when the plan depends on an unknown only research can settle (unfamiliar code, unclear root cause, a feasibility question), start with a read-only agent that explores and reports findings as outputs. It proposes; it does not edit. Feed its outputs into the implementation prompt.
- Independent review: when a plausible, self-consistent change could still be wrong in ways its author would not notice (concurrency, security, migrations, data loss, shared interfaces), add a review by a node that did not write the code. When more than one suitable model is allowed, review with a different model than the implementer's.

Escalate only for those reasons: an unresolved unknown earns an investigation, and an author-blind failure mode earns an independent review. Importance alone earns neither. Name the level you chose, and why, in your explanation.

# Rules

- Every node needs outcomes and a next entry for each outcome (parallel nodes excepted).
- Use parallel for independent read-only work that would otherwise wait in line: lint + tests + review, reviews by two different models, or several investigations. Put edits after the join; route the join's failure outcome to a fixer and back to the parallel node for another round (bounded with max_visits).
- End where the task's deliverable is. A pull request is right for code changes; investigations, questions and reports end with comment_task or $success once their outputs exist. Do not add a PR the task does not need.
- Loops are good (tests fail → fix again), but every loop needs max_visits on one of its nodes.
- For "repeat until" goals, prefer a bounded work → verify → condition group over a fixed one-pass chain. Route the condition's false/default outcome back to work and true to the next stage; refresh verification on EVERY pass. Use direct check outcomes for executable criteria. For judgment criteria, declare a bool output such as goal_met on a reviewer and route with CEL: nodes.verify.outcome == "pass" && nodes.assess.outputs.goal_met == true. An LLM's judgment must not override failed checks.
- There are no shared mutable variables: nodes.<id>.outputs contains the latest successful visit's persisted result. nodes.<id>.history lists every successful visit (visit, outcome, summary, outputs), oldest first; use it for all earlier feedback in a loop or size(nodes.<id>.history) in a switch. Steps in a loop already get every earlier pass in their context. Do not read a producer before it has run; on an initial work visit use template fallbacks for prior feedback. Bind named outputs explicitly when a switch or gate is the previous step.
- max_visits is a per-node total for the run, not a group iteration variable, and does not reset on a back edge or human decision. Bound every cycle, account for preset limits on all nodes in the repeated group, and explicitly route on_exhausted to $fail or a bounded handoff gate.
- For human revise/approve loops, route revise back to work and approve onward. A gate alone does NOT automatically bound repetition: set max_visits plus an explicit gate timeout and next.timeout (normally $fail). Catalog/project/flow pod timeout defaults do not apply to gates. A gate timeout is per wait, not a whole-loop deadline; budget.wall is not currently enforced by the engine. The human can add a free-text note with the decision: the next step sees it in its context automatically, and later steps can read ${{ nodes.<gate>.outputs.note ?? "" }}. Route revise straight back to the work node so the note arrives with it. Gates remain optional according to guidance.
- Nodes automatically receive the task, a summary of earlier steps, and the previous step's outputs; do not repeat them in prompts. Use ${{ nodes.<id>.outputs.<field> }} or ${{ task.title }} only when a step needs a specific value.
- Prefer presets and the smallest sufficient configured model for each step.
- Use the configured planner for decomposition and routing. Only select models in the menu; do not assume a local-model preference.
- Requires hints describe preconditions, not grants or automatic setup. llm nodes cannot inspect files or run tools; use an agent to gather missing evidence.
- Each pod gets a fresh checkout; installed dependencies and uncommitted files do not carry between nodes. Checks need dependencies in their runtime or setup in their own run command, with permitted network access. Never invent tools or use a passing placeholder check.
- Override preset run for repo-specific commands. Preset maps (inputs, with, outputs, cases) are replaced, not merged; supply the complete map/list when overriding. Wire earlier outputs explicitly when they are not from the immediately previous step.
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
      model: gpt-6.1-sol
      grants: [repo/example:write]
      max_visits: 3
      on_exhausted: $fail
      next: { done: test, stuck: $fail }
    test:
      uses: preset/python-unittest
      next: { pass: review, fail: implement }
    review:
      uses: preset/code-review
      model: gpt-6-luna
      next: { approve: open_pr, changes: implement }
    open_pr:
      uses: preset/open-pull-request
      next: { done: $success }
`

// writeDeploy explains a project that ships to production on merge.
func writeDeploy(sb *strings.Builder, base string, d *config.Deploy) {
	fmt.Fprintf(sb, "\n## Deploys on merge\n\nMerging a PR into `%s` ships to production: CI/CD builds and rolls it out", d.Branch)
	if d.URL != "" {
		fmt.Fprintf(sb, " (%s)", d.URL)
	}
	sb.WriteString(".")
	if d.Branch != base {
		fmt.Fprintf(sb, " This project's flows target `%s`, so merging them does not deploy.\n", base)
		return
	}
	sb.WriteString(` When the task should reach production, build a release pipeline:

1. work and its local verification (tests, lint, local-stack-test; qa-test for user-facing changes), in parallel where independent;
2. open-pull-request, then ci-checks (passed → on; failed → back to the fix step);
3. deploy (merge) → wait-for-deploy → monitor-deploy;
4. healthy → comment-task or $success; degraded or timeout → deploy-triage → revert-deploy (repo write grant) → open-pull-request → ci-checks → deploy → $fail, so the run reports the rollback.

The validator enforces that every deploy follows a passing ci-checks with no repo-writing step after it, and that after a deploy the run only succeeds through wait-for-deploy then monitor-deploy (healthy). Tasks that should not ship (investigations, drafts) end before deploy.
`)
}
