package resolve

import (
	"github.com/mauza/ai-flow/internal/flow"
)

// Graph is the UI's view of a flow: effective node settings and labelled edges.
type Graph struct {
	Start string      `json:"start"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type GraphNode struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Preset      string            `json:"preset,omitempty"`
	Description string            `json:"description,omitempty"`
	Model       string            `json:"model,omitempty"`
	Fallbacks   []string          `json:"fallbacks,omitempty"`
	Harness     string            `json:"harness,omitempty"`
	Runtime     string            `json:"runtime,omitempty"`
	Grants      []string          `json:"grants,omitempty"`
	Skills      []string          `json:"skills,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Run         string            `json:"run,omitempty"`
	Action      string            `json:"action,omitempty"`
	Cases       []flow.Case       `json:"cases,omitempty"`
	Default     string            `json:"default,omitempty"`
	Outcomes    []string          `json:"outcomes"`
	Outputs     map[string]any    `json:"outputs,omitempty"`
	MaxVisits   int               `json:"max_visits,omitempty"`
	OnExhausted string            `json:"on_exhausted,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
	Limits      *flow.Limits      `json:"limits,omitempty"`
	OnLimit     map[string]string `json:"on_limit,omitempty"`
}

type GraphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Outcome string `json:"outcome"`
	Kind    string `json:"kind"` // next | exhausted
}

// Graph builds the UI projection. Edges to unknown nodes are kept so the UI
// can show them as broken.
func (r *Resolved) Graph() *Graph {
	g := &Graph{Start: r.Flow.Spec.Start, Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	for _, id := range r.Order {
		n := r.Nodes[id]
		gn := GraphNode{
			ID: id, Type: n.Type, Preset: n.Preset, Description: n.Description, Model: n.Model,
			Harness: n.Harness, Runtime: n.Runtime, Grants: n.Grants, Skills: n.Skills, Prompt: n.Prompt,
			Run: n.Run, Action: n.Action, Cases: n.Cases, Default: n.Default, Outcomes: n.Outcomes,
			Outputs: n.Outputs, MaxVisits: n.MaxVisits, OnExhausted: n.OnExhausted,
		}
		if gn.Outcomes == nil {
			gn.Outcomes = []string{}
		}
		if n.Timeout.Duration > 0 {
			gn.Timeout = n.Timeout.Duration.String()
		}
		if n.LLM != nil {
			gn.Fallbacks = n.LLM.Fallbacks
			gn.Limits = n.LLM.Limits
			gn.OnLimit = map[string]string{}
			for k, ol := range n.LLM.OnLimit {
				if ol != nil {
					s := ol.Action
					if ol.Then != "" {
						s += " → " + ol.Then
					}
					gn.OnLimit[k] = s
				}
			}
		}
		g.Nodes = append(g.Nodes, gn)
		for _, o := range n.Outcomes {
			if t, ok := n.Next[o]; ok {
				g.Edges = append(g.Edges, GraphEdge{From: id, To: t, Outcome: o, Kind: "next"})
			}
		}
		if n.MaxVisits > 0 && n.OnExhausted != "" {
			g.Edges = append(g.Edges, GraphEdge{From: id, To: n.OnExhausted, Outcome: "exhausted", Kind: "exhausted"})
		}
	}
	return g
}
