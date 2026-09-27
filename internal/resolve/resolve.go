// Package resolve turns a flow document into effective per-node settings
// (presets and defaults applied) and validates it against the catalog and
// project. The same code backs the planner, the UI, the CLI and the runtime.
package resolve

import (
	"sort"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

// Resolved is a flow with every node's effective settings.
type Resolved struct {
	Flow    *flow.Flow
	Project *config.Project
	Repo    string // git grant name
	Base    string
	Nodes   map[string]*Node
	Order   []string
}

// Node is the effective configuration of one node.
type Node struct {
	ID     string
	Preset string
	flow.Node
}

// Resolve applies presets and defaults. It never fails; problems surface in Validate.
func Resolve(f *flow.Flow, cfg *config.Config) *Resolved {
	var proj *config.Project
	if f.Metadata.Project != "" {
		proj = cfg.Projects[f.Metadata.Project]
	}
	r := &Resolved{Flow: f, Project: proj, Nodes: map[string]*Node{}}
	if f.Spec.Repo != nil {
		r.Repo, r.Base = f.Spec.Repo.Grant, f.Spec.Repo.Base
	}
	if proj != nil {
		if r.Repo == "" {
			r.Repo = proj.Spec.Repo
		}
		if r.Base == "" {
			r.Base = proj.Spec.Base
		}
	}
	if r.Base == "" {
		r.Base = "main"
	}

	for id, raw := range f.Spec.Nodes {
		n := &Node{ID: id}
		n.Node = *cloneNode(raw)
		if raw.Uses != "" {
			name := strings.TrimPrefix(raw.Uses, "preset/")
			n.Preset = name
			if p, ok := cfg.Catalog.Presets[name]; ok {
				fill(&n.Node, &p.Node)
			}
		}
		fillDefaults(&n.Node, f.Spec.Defaults)
		if proj != nil {
			fillDefaults(&n.Node, proj.Spec.Defaults)
		}
		fillDefaults(&n.Node, cfg.Catalog.Defaults)
		if n.Harness == "" && n.Type == flow.TypeAgent {
			n.Harness = "pi"
		}
		if n.Timeout.Duration == 0 && flow.PodType(n.Type) {
			n.Timeout.Duration = cfg.Env.Runs.DefaultTimeout.Duration
		}
		if n.OnExhausted == "" && n.MaxVisits > 0 {
			n.OnExhausted = flow.Fail
		}
		if n.Type == flow.TypeLLM || n.Type == flow.TypeAgent {
			n.LLM = effectiveLLM(&n.Node, cfg)
			n.Model = n.LLM.Model
		}
		n.Outcomes = effectiveOutcomes(&n.Node)
		r.Nodes[id] = n
	}
	r.Order = f.NodeIDs()
	return r
}

func cloneNode(n *flow.Node) *flow.Node {
	if n == nil {
		return &flow.Node{}
	}
	c := *n
	if n.LLM != nil {
		l := *n.LLM
		c.LLM = &l
	}
	return &c
}

// fillDefaults applies the execution settings of a defaults block. Defaults only
// shape how pod nodes run; they never add prompts, outcomes or transitions, and
// a default timeout must not turn into a gate timeout.
func fillDefaults(dst, src *flow.Node) {
	if src == nil || !flow.PodType(dst.Type) {
		return
	}
	d := &flow.Node{Model: src.Model, Runtime: src.Runtime, Timeout: src.Timeout, Retry: src.Retry, LLM: src.LLM}
	if dst.Type == flow.TypeAgent {
		d.Harness = src.Harness
	}
	fill(dst, d)
}

// fill copies src fields into dst where dst has the zero value.
func fill(dst, src *flow.Node) {
	if src == nil {
		return
	}
	str := func(d *string, s string) {
		if *d == "" {
			*d = s
		}
	}
	str(&dst.Type, src.Type)
	str(&dst.Description, src.Description)
	str(&dst.Model, src.Model)
	str(&dst.Harness, src.Harness)
	str(&dst.Runtime, src.Runtime)
	str(&dst.Prompt, src.Prompt)
	str(&dst.OnExhausted, src.OnExhausted)
	str(&dst.Run, src.Run)
	str(&dst.Default, src.Default)
	str(&dst.Action, src.Action)
	if dst.Grants == nil {
		dst.Grants = src.Grants
	}
	if dst.Skills == nil {
		dst.Skills = src.Skills
	}
	if dst.Inputs == nil {
		dst.Inputs = src.Inputs
	}
	if dst.Outputs == nil {
		dst.Outputs = src.Outputs
	}
	if dst.Outcomes == nil {
		dst.Outcomes = src.Outcomes
	}
	if dst.Next == nil {
		dst.Next = src.Next
	}
	if dst.ExitCodes == nil {
		dst.ExitCodes = src.ExitCodes
	}
	if dst.Cases == nil {
		dst.Cases = src.Cases
	}
	if dst.With == nil {
		dst.With = src.With
	}
	if dst.MaxVisits == 0 {
		dst.MaxVisits = src.MaxVisits
	}
	if dst.Timeout.Duration == 0 {
		dst.Timeout = src.Timeout
	}
	if dst.Retry == nil {
		dst.Retry = src.Retry
	}
	if src.LLM != nil {
		if dst.LLM == nil {
			l := *src.LLM
			dst.LLM = &l
		} else {
			fillLLM(dst.LLM, src.LLM)
		}
	}
}

func fillLLM(dst, src *flow.LLMConfig) {
	if src == nil {
		return
	}
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.Fallbacks == nil {
		dst.Fallbacks = src.Fallbacks
	}
	if dst.Thinking == "" {
		dst.Thinking = src.Thinking
	}
	if dst.Limits == nil {
		dst.Limits = src.Limits
	}
	if len(src.OnLimit) > 0 {
		if dst.OnLimit == nil {
			dst.OnLimit = map[string]*flow.OnLimit{}
		}
		for k, v := range src.OnLimit {
			if _, ok := dst.OnLimit[k]; !ok {
				dst.OnLimit[k] = v
			}
		}
	}
}

// DefaultOnLimit applies when neither node, preset, defaults nor model say otherwise.
var DefaultOnLimit = map[string]*flow.OnLimit{
	flow.LimitRateLimited:     {Action: flow.ActRetry, Max: 5, Backoff: flow.Duration{Duration: 20 * time.Second}, Then: flow.ActFail},
	flow.LimitQuotaExhausted:  {Action: flow.ActWait, MaxWait: flow.Duration{Duration: 30 * time.Minute}, Then: flow.ActFail},
	flow.LimitContextExceeded: {Action: flow.ActFail},
	flow.LimitBudgetExceeded:  {Action: flow.ActFail},
}

func effectiveLLM(n *flow.Node, cfg *config.Config) *flow.LLMConfig {
	l := &flow.LLMConfig{}
	if n.LLM != nil {
		*l = *n.LLM
		if n.LLM.OnLimit != nil {
			l.OnLimit = map[string]*flow.OnLimit{}
			for k, v := range n.LLM.OnLimit {
				l.OnLimit[k] = v
			}
		}
	}
	if l.Model == "" {
		l.Model = n.Model
	}
	if m, ok := cfg.Catalog.Models[l.Model]; ok {
		fillLLM(l, m.LLM)
	}
	fillLLM(l, &flow.LLMConfig{OnLimit: DefaultOnLimit})
	return l
}

func effectiveOutcomes(n *flow.Node) []string {
	var out []string
	add := func(o string) {
		if o != "" && !contains(out, o) {
			out = append(out, o)
		}
	}
	for _, o := range n.Outcomes {
		add(o)
	}
	switch n.Type {
	case flow.TypeCheck:
		if len(n.Outcomes) == 0 {
			if len(n.ExitCodes) == 0 {
				add("pass")
				add("fail")
			}
			keys := make([]string, 0, len(n.ExitCodes))
			for k := range n.ExitCodes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				add(n.ExitCodes[k])
			}
		}
	case flow.TypeSwitch:
		if len(n.Outcomes) == 0 {
			for _, c := range n.Cases {
				add(c.Outcome)
			}
			add(n.Default)
		}
	case flow.TypeAction:
		if len(out) == 0 {
			add("done")
		}
	case flow.TypeGate:
		if n.Timeout.Duration > 0 {
			add(flow.OutcomeTimeout)
		}
	case flow.TypeLLM, flow.TypeAgent:
		if n.LLM != nil {
			for _, ol := range n.LLM.OnLimit {
				if ol != nil && (ol.Action == flow.ActOutcome || ol.Then == flow.ActOutcome) {
					add(flow.OutcomeLimit)
				}
			}
		}
	}
	return out
}

// CheckExitCodes returns exit code → outcome with defaults filled in.
func CheckExitCodes(n *flow.Node) map[string]string {
	if len(n.ExitCodes) > 0 {
		return n.ExitCodes
	}
	return map[string]string{"0": "pass", "default": "fail"}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Targets returns every transition target of n, including on_exhausted.
func (n *Node) Targets() []string {
	var t []string
	for _, o := range n.Outcomes {
		if v, ok := n.Next[o]; ok {
			t = append(t, v)
		}
	}
	if n.MaxVisits > 0 && n.OnExhausted != "" {
		t = append(t, n.OnExhausted)
	}
	return t
}
