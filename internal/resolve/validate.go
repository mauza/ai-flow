package resolve

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/tmpl"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Issue is one validation finding. Node is empty for flow-level issues.
type Issue struct {
	Severity Severity `json:"severity"`
	Node     string   `json:"node,omitempty"`
	Field    string   `json:"field,omitempty"`
	Message  string   `json:"message"`
}

func (i Issue) String() string {
	loc := "flow"
	if i.Node != "" {
		loc = "node " + i.Node
	}
	if i.Field != "" {
		loc += "." + i.Field
	}
	return fmt.Sprintf("%s: %s: %s", i.Severity, loc, i.Message)
}

// HasErrors reports whether any issue is an error.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == Error {
			return true
		}
	}
	return false
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	nodeIDRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	outcomeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
)

type validator struct {
	r      *Resolved
	cfg    *config.Config
	issues []Issue
}

func (v *validator) errf(node, field, format string, a ...any) {
	v.issues = append(v.issues, Issue{Error, node, field, fmt.Sprintf(format, a...)})
}

func (v *validator) warnf(node, field, format string, a ...any) {
	v.issues = append(v.issues, Issue{Warning, node, field, fmt.Sprintf(format, a...)})
}

// Validate checks a resolved flow. Issues are sorted: flow-level first, then by node order.
func Validate(r *Resolved, cfg *config.Config) []Issue {
	v := &validator{r: r, cfg: cfg}
	f := r.Flow

	if f.Kind != "" && f.Kind != flow.Kind {
		v.errf("", "kind", "must be %q", flow.Kind)
	}
	if !nameRe.MatchString(f.Metadata.Name) {
		v.errf("", "metadata.name", "must be lowercase letters, digits and dashes (got %q)", f.Metadata.Name)
	}
	if f.Metadata.Project != "" && r.Project == nil {
		v.errf("", "metadata.project", "unknown project %q", f.Metadata.Project)
	}
	if s := f.Metadata.Start; s != "" && s != "manual" && s != "auto" {
		v.errf("", "metadata.start", "must be manual or auto")
	}
	if len(f.Spec.Nodes) == 0 {
		v.errf("", "spec.nodes", "a flow needs at least one node")
		return v.sorted()
	}
	if f.Spec.Start == "" {
		v.errf("", "spec.start", "required")
	} else if _, ok := f.Spec.Nodes[f.Spec.Start]; !ok {
		v.errf("", "spec.start", "unknown node %q", f.Spec.Start)
	}
	v.checkRepo()
	if b := f.Spec.Budget; b != nil && r.Project != nil && r.Project.Spec.Budget != nil {
		if max := r.Project.Spec.Budget.USDPerRun; max > 0 && b.USD > max {
			v.errf("", "spec.budget.usd", "$%.2f exceeds the project cap of $%.2f per run", b.USD, max)
		}
	}

	for _, id := range r.Order {
		v.checkNode(r.Nodes[id], f.Spec.Nodes[id])
	}
	v.checkGraph()
	return v.sorted()
}

func (v *validator) sorted() []Issue {
	pos := map[string]int{"": -1}
	for i, id := range v.r.Order {
		pos[id] = i
	}
	sort.SliceStable(v.issues, func(a, b int) bool {
		return pos[v.issues[a].Node] < pos[v.issues[b].Node]
	})
	return v.issues
}

func (v *validator) checkRepo() {
	r := v.r
	if r.Repo == "" {
		return
	}
	g, ok := v.cfg.Catalog.Grants[r.Repo]
	if !ok {
		v.errf("", "spec.repo.grant", "unknown grant %q", r.Repo)
		return
	}
	if g.Kind != config.GrantGit {
		v.errf("", "spec.repo.grant", "%q is a %s grant, not git", r.Repo, g.Kind)
	}
	if !r.Project.AllowsGrant(r.Repo + ":read") {
		v.errf("", "spec.repo.grant", "project does not allow %q", r.Repo)
	}
}

func (v *validator) checkNode(n *Node, raw *flow.Node) {
	id := n.ID
	cat := &v.cfg.Catalog
	if !nodeIDRe.MatchString(id) {
		v.errf(id, "", "node ids must be lowercase letters, digits and underscores, starting with a letter")
	}
	if raw.Uses != "" {
		if _, ok := cat.Presets[n.Preset]; !ok {
			v.errf(id, "uses", "unknown preset %q", raw.Uses)
		}
	}
	if !contains(flow.NodeTypes, n.Type) {
		if n.Type == "" {
			v.errf(id, "type", "required (one of %s)", strings.Join(flow.NodeTypes, ", "))
		} else {
			v.errf(id, "type", "unknown type %q (one of %s)", n.Type, strings.Join(flow.NodeTypes, ", "))
		}
		return
	}

	// outcomes + transitions
	if len(n.Outcomes) == 0 {
		v.errf(id, "outcomes", "declare at least one outcome")
	}
	seen := map[string]bool{}
	for _, o := range n.Outcomes {
		if !outcomeRe.MatchString(o) {
			v.errf(id, "outcomes", "outcome %q must be lowercase letters, digits and underscores", o)
		}
		if seen[o] {
			v.errf(id, "outcomes", "duplicate outcome %q", o)
		}
		seen[o] = true
		t, ok := n.Next[o]
		if !ok {
			v.errf(id, "next", "outcome %q has no transition", o)
			continue
		}
		v.checkTarget(id, "next."+o, t)
	}
	for o := range n.Next {
		if !seen[o] {
			v.errf(id, "next."+o, "%q is not one of this node's outcomes (%s)", o, strings.Join(n.Outcomes, ", "))
		}
	}
	if n.MaxVisits < 0 {
		v.errf(id, "max_visits", "must be positive")
	}
	if n.MaxVisits > 0 {
		v.checkTarget(id, "on_exhausted", n.OnExhausted)
	} else if raw.OnExhausted != "" {
		v.warnf(id, "on_exhausted", "has no effect without max_visits")
	}

	switch n.Type {
	case flow.TypeLLM, flow.TypeAgent:
		v.checkModelNode(n)
	case flow.TypeCheck:
		if strings.TrimSpace(n.Run) == "" {
			v.errf(id, "run", "check nodes need a command to run")
		}
		v.checkRuntime(n)
		for code, o := range CheckExitCodes(&n.Node) {
			if code != "default" {
				if _, err := strconv.Atoi(code); err != nil {
					v.errf(id, "exit_codes", "key %q must be an exit code or \"default\"", code)
				}
			}
			if !contains(n.Outcomes, o) {
				v.errf(id, "exit_codes", "maps to %q, which is not an outcome", o)
			}
		}
	case flow.TypeGate:
		if strings.TrimSpace(n.Prompt) == "" {
			v.warnf(id, "prompt", "tell the human what they are deciding")
		}
	case flow.TypeSwitch:
		if len(n.Cases) == 0 {
			v.errf(id, "cases", "switch nodes need at least one case")
		}
		for i, c := range n.Cases {
			if err := CompileCEL(c.When); err != nil {
				v.errf(id, fmt.Sprintf("cases[%d].when", i), "%v", err)
			}
			if !contains(n.Outcomes, c.Outcome) {
				v.errf(id, fmt.Sprintf("cases[%d].outcome", i), "%q is not an outcome", c.Outcome)
			}
		}
		if n.Default == "" {
			v.errf(id, "default", "required: the outcome when no case matches")
		} else if !contains(n.Outcomes, n.Default) {
			v.errf(id, "default", "%q is not an outcome", n.Default)
		}
	case flow.TypeAction:
		if !contains(cat.Actions, n.Action) {
			v.errf(id, "action", "unknown action %q (one of %s)", n.Action, strings.Join(cat.Actions, ", "))
		}
		if n.Action == "open_pull_request" && v.r.Repo == "" {
			v.errf(id, "action", "open_pull_request needs spec.repo or a project repo")
		}
	}

	if !flow.PodType(n.Type) && len(raw.Grants) > 0 {
		v.warnf(id, "grants", "%s nodes run in the control plane; grants are ignored", n.Type)
	}
	if flow.PodType(n.Type) {
		v.checkGrants(n)
	}
	v.checkTemplates(n)
}

func (v *validator) checkTarget(node, field, t string) {
	if flow.Terminal(t) {
		return
	}
	if t == "" {
		v.errf(node, field, "missing target")
		return
	}
	if _, ok := v.r.Flow.Spec.Nodes[t]; !ok {
		v.errf(node, field, "unknown node %q (use %s or %s to end the run)", t, flow.Success, flow.Fail)
	}
}

func (v *validator) checkModelNode(n *Node) {
	id := n.ID
	cat := &v.cfg.Catalog
	if strings.TrimSpace(n.Prompt) == "" {
		v.errf(id, "prompt", "required")
	}
	models := append([]string{n.LLM.Model}, n.LLM.Fallbacks...)
	for i, m := range models {
		field := "model"
		if i > 0 {
			field = "llm.fallbacks"
		}
		if m == "" {
			v.errf(id, field, "required: pick a model from the catalog")
			continue
		}
		if _, ok := cat.Models[m]; !ok {
			v.errf(id, field, "unknown model %q", m)
		} else if !v.r.Project.AllowsModel(m) {
			v.errf(id, field, "project does not allow model %q", m)
		}
	}
	for kind, ol := range n.LLM.OnLimit {
		if !contains(flow.LimitKinds, kind) {
			v.errf(id, "llm.on_limit."+kind, "unknown limit kind (one of %s)", strings.Join(flow.LimitKinds, ", "))
			continue
		}
		if ol == nil || !contains(flow.LimitActions, ol.Action) {
			v.errf(id, "llm.on_limit."+kind, "action must be one of %s", strings.Join(flow.LimitActions, ", "))
			continue
		}
		if ol.Then != "" && !contains(flow.LimitActions, ol.Then) {
			v.errf(id, "llm.on_limit."+kind+".then", "must be one of %s", strings.Join(flow.LimitActions, ", "))
		}
		if (ol.Action == flow.ActFallback || ol.Then == flow.ActFallback) && len(n.LLM.Fallbacks) == 0 {
			v.warnf(id, "llm.on_limit."+kind, "fallback without llm.fallbacks behaves like fail")
		}
	}
	if n.Type == flow.TypeAgent {
		if _, ok := cat.Harnesses[n.Harness]; !ok {
			v.errf(id, "harness", "unknown harness %q", n.Harness)
		}
		for _, s := range n.Skills {
			if _, ok := cat.Skills[s]; !ok {
				v.errf(id, "skills", "unknown skill %q", s)
			}
		}
	}
	v.checkRuntime(n)
	for name, typ := range n.Outputs {
		if name == "outcome" {
			v.errf(id, "outputs", "\"outcome\" is reserved")
		}
		if _, err := OutputSchema(typ); err != nil {
			v.errf(id, "outputs."+name, "%v", err)
		}
	}
}

func (v *validator) checkRuntime(n *Node) {
	if n.Runtime == "" {
		v.errf(n.ID, "runtime", "required: pick a runtime image from the catalog")
		return
	}
	if _, ok := v.cfg.Catalog.Runtimes[n.Runtime]; !ok {
		v.errf(n.ID, "runtime", "unknown runtime %q", n.Runtime)
	}
}

func (v *validator) checkGrants(n *Node) {
	for _, g := range n.Grants {
		name, mode, _ := strings.Cut(g, ":")
		grant, ok := v.cfg.Catalog.Grants[name]
		if !ok {
			v.errf(n.ID, "grants", "unknown grant %q", name)
			continue
		}
		if !v.r.Project.AllowsGrant(g) {
			v.errf(n.ID, "grants", "project does not allow %q", g)
		}
		switch grant.Kind {
		case config.GrantGit:
			if mode == "" {
				mode = "read"
			}
			if mode != "read" && mode != "write" {
				v.errf(n.ID, "grants", "%q: git mode must be read or write", g)
			} else if len(grant.Modes) > 0 && !contains(grant.Modes, mode) {
				v.errf(n.ID, "grants", "%q: grant only allows %s", g, strings.Join(grant.Modes, ", "))
			}
			if name != v.r.Repo {
				v.warnf(n.ID, "grants", "%q is not the flow's repo; only the flow repo is checked out", name)
			}
		case config.GrantMCP:
			if n.Type != flow.TypeAgent {
				v.warnf(n.ID, "grants", "%q: only agent nodes can call MCP tools", g)
			}
		}
	}
}

func (v *validator) checkTemplates(n *Node) {
	check := func(field, s string) {
		refs, errs := tmpl.Refs(s)
		for _, err := range errs {
			v.errf(n.ID, field, "%v", err)
		}
		for _, ref := range refs {
			v.checkRef(n, field, ref)
		}
	}
	check("prompt", n.Prompt)
	for k, s := range n.Inputs {
		check("inputs."+k, s)
	}
	for k, val := range n.With {
		if s, ok := val.(string); ok {
			check("with."+k, s)
		}
	}
}

var refRoots = []string{"task", "run", "nodes", "inputs"}

func (v *validator) checkRef(n *Node, field string, ref tmpl.Ref) {
	root := ref.Path[0]
	if !contains(refRoots, root) {
		v.errf(n.ID, field, "%s: unknown root %q (one of %s)", ref.Raw, root, strings.Join(refRoots, ", "))
		return
	}
	switch root {
	case "inputs":
		if len(ref.Path) < 2 {
			v.errf(n.ID, field, "%s: name an input", ref.Raw)
		} else if _, ok := n.Inputs[ref.Path[1]]; !ok {
			v.errf(n.ID, field, "%s: node has no input %q", ref.Raw, ref.Path[1])
		}
	case "nodes":
		if len(ref.Path) < 3 {
			v.errf(n.ID, field, "%s: use nodes.<id>.outputs.<field> or nodes.<id>.outcome", ref.Raw)
			return
		}
		other, ok := v.r.Nodes[ref.Path[1]]
		if !ok {
			v.errf(n.ID, field, "%s: unknown node %q", ref.Raw, ref.Path[1])
			return
		}
		switch ref.Path[2] {
		case "outcome", "summary":
		case "outputs":
			if len(ref.Path) >= 4 && other.Outputs != nil {
				if _, ok := other.Outputs[ref.Path[3]]; !ok && other.Type != flow.TypeCheck {
					v.warnf(n.ID, field, "%s: node %q declares no output %q", ref.Raw, other.ID, ref.Path[3])
				}
			}
		default:
			v.errf(n.ID, field, "%s: expected outputs, outcome or summary after the node id", ref.Raw)
		}
	}
}

// checkGraph: reachability, termination, bounded cycles.
func (v *validator) checkGraph() {
	r := v.r
	start := r.Flow.Spec.Start
	if _, ok := r.Nodes[start]; !ok {
		return
	}
	// reachable from start
	reach := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if reach[id] || flow.Terminal(id) {
			return
		}
		n, ok := r.Nodes[id]
		if !ok {
			return
		}
		reach[id] = true
		for _, t := range n.Targets() {
			walk(t)
		}
	}
	walk(start)
	for _, id := range r.Order {
		if !reach[id] {
			v.errf(id, "", "unreachable from start node %q", start)
		}
	}

	// every node can reach a terminal
	canEnd := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for id, n := range r.Nodes {
			if canEnd[id] {
				continue
			}
			for _, t := range n.Targets() {
				if flow.Terminal(t) || canEnd[t] {
					canEnd[id] = true
					changed = true
					break
				}
			}
		}
	}
	for _, id := range r.Order {
		if reach[id] && !canEnd[id] {
			v.errf(id, "next", "no path from here reaches %s or %s", flow.Success, flow.Fail)
		}
	}

	// cycles must pass through a bounded node (max_visits) or a human gate
	bounded := func(id string) bool {
		n := r.Nodes[id]
		return n.MaxVisits > 0 || n.Type == flow.TypeGate
	}
	state := map[string]int{} // 0 new, 1 on stack, 2 done
	var stack []string
	reported := map[string]bool{}
	var dfs func(string)
	dfs = func(id string) {
		state[id] = 1
		stack = append(stack, id)
		for _, t := range r.Nodes[id].Targets() {
			if _, ok := r.Nodes[t]; !ok || bounded(t) {
				continue
			}
			switch state[t] {
			case 0:
				dfs(t)
			case 1:
				i := len(stack) - 1
				for i >= 0 && stack[i] != t {
					i--
				}
				cycle := append(append([]string{}, stack[i:]...), t)
				if !reported[t] {
					reported[t] = true
					v.errf(t, "max_visits", "unbounded loop %s: set max_visits on one of these nodes", strings.Join(cycle, " → "))
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
	}
	for _, id := range r.Order {
		if !bounded(id) && state[id] == 0 {
			dfs(id)
		}
	}
}
