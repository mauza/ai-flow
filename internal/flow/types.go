// Package flow defines the flow document: a per-task state machine whose nodes
// emit one outcome each, and whose transitions map outcomes to the next node.
package flow

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	APIVersion = "ai-flow/v1alpha1"
	Kind       = "Flow"

	Success = "$success"
	Fail    = "$fail"

	// Outcomes added implicitly by the runtime.
	OutcomeTimeout = "timeout"
	OutcomeLimit   = "limit"
)

// Node types.
const (
	TypeLLM    = "llm"
	TypeAgent  = "agent"
	TypeCheck  = "check"
	TypeGate   = "gate"
	TypeSwitch = "switch"
	TypeAction = "action"
)

var NodeTypes = []string{TypeLLM, TypeAgent, TypeCheck, TypeGate, TypeSwitch, TypeAction}

// PodTypes run as a Kubernetes Job; the rest run inside the control plane.
func PodType(t string) bool { return t == TypeLLM || t == TypeAgent || t == TypeCheck }

type Flow struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name      string   `json:"name"`
	Project   string   `json:"project,omitempty"`
	Task      *TaskRef `json:"task,omitempty"`
	Version   int      `json:"version,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	Start     string   `json:"start,omitempty"` // manual | auto; overrides the project
}

type TaskRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

type Spec struct {
	Description string           `json:"description,omitempty"`
	Repo        *Repo            `json:"repo,omitempty"`
	Budget      *Budget          `json:"budget,omitempty"`
	Defaults    *Node            `json:"defaults,omitempty"`
	Start       string           `json:"start"`
	Nodes       map[string]*Node `json:"nodes"`
}

type Repo struct {
	Grant string `json:"grant"`
	Base  string `json:"base,omitempty"`
}

type Budget struct {
	USD  float64  `json:"usd,omitempty"`
	Wall Duration `json:"wall,omitzero"`
}

type Node struct {
	Type        string            `json:"type,omitempty"`
	Uses        string            `json:"uses,omitempty"`
	Description string            `json:"description,omitempty"`
	Model       string            `json:"model,omitempty"`
	LLM         *LLMConfig        `json:"llm,omitempty"`
	Harness     string            `json:"harness,omitempty"`
	Runtime     string            `json:"runtime,omitempty"`
	Grants      []string          `json:"grants,omitempty"`
	Skills      []string          `json:"skills,omitempty"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Prompt      string            `json:"prompt,omitempty"`
	Outputs     map[string]any    `json:"outputs,omitempty"`
	Outcomes    []string          `json:"outcomes,omitempty"`
	Next        map[string]string `json:"next,omitempty"`
	MaxVisits   int               `json:"max_visits,omitempty"`
	OnExhausted string            `json:"on_exhausted,omitempty"`
	Timeout     Duration          `json:"timeout,omitzero"`
	Retry       *Retry            `json:"retry,omitempty"`

	// check
	Run       string            `json:"run,omitempty"`
	ExitCodes map[string]string `json:"exit_codes,omitempty"`

	// switch
	Cases   []Case `json:"cases,omitempty"`
	Default string `json:"default,omitempty"`

	// action
	Action string         `json:"action,omitempty"`
	With   map[string]any `json:"with,omitempty"`
}

type Case struct {
	When    string `json:"when"`
	Outcome string `json:"outcome"`
}

type Retry struct {
	Limit int `json:"limit"`
}

// LLMConfig is the full per-step model configuration (spec §4.5).
type LLMConfig struct {
	Model     string              `json:"model,omitempty"`
	Fallbacks []string            `json:"fallbacks,omitempty"`
	Thinking  string              `json:"thinking,omitempty"`
	Limits    *Limits             `json:"limits,omitempty"`
	OnLimit   map[string]*OnLimit `json:"on_limit,omitempty"`
}

type Limits struct {
	Tokens int     `json:"tokens,omitempty"`
	USD    float64 `json:"usd,omitempty"`
	Turns  int     `json:"turns,omitempty"`
}

// Limit kinds understood by on_limit.
const (
	LimitRateLimited     = "rate_limited"
	LimitQuotaExhausted  = "quota_exhausted"
	LimitContextExceeded = "context_exceeded"
	LimitBudgetExceeded  = "budget_exceeded"
)

var LimitKinds = []string{LimitRateLimited, LimitQuotaExhausted, LimitContextExceeded, LimitBudgetExceeded}

// On-limit actions.
const (
	ActRetry    = "retry"
	ActWait     = "wait"
	ActFallback = "fallback"
	ActOutcome  = "outcome"
	ActFail     = "fail"
)

var LimitActions = []string{ActRetry, ActWait, ActFallback, ActOutcome, ActFail}

type OnLimit struct {
	Action  string   `json:"action"`
	Max     int      `json:"max,omitempty"`
	Backoff Duration `json:"backoff,omitzero"`
	MaxWait Duration `json:"max_wait,omitzero"`
	Then    string   `json:"then,omitempty"`
}

// Duration marshals as a Go duration string ("30m", "2h").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) {
	if d.Duration == 0 {
		return []byte(`""`), nil
	}
	return []byte(fmt.Sprintf("%q", shortDuration(d.Duration))), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		d.Duration = 0
		return nil
	}
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			d.Duration = time.Duration(days) * 24 * time.Hour
			return nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	d.Duration = v
	return nil
}

// shortDuration renders 2h0m0s as 2h and 30m0s as 30m, leaving 30s alone.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Parse decodes a YAML or JSON flow document.
func Parse(data []byte) (*Flow, error) {
	var f Flow
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("parse flow: %w", cleanYAMLError(err))
	}
	if f.Spec.Nodes == nil {
		f.Spec.Nodes = map[string]*Node{}
	}
	for id, n := range f.Spec.Nodes {
		if n == nil {
			f.Spec.Nodes[id] = &Node{}
		}
	}
	return &f, nil
}

func cleanYAMLError(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "error converting YAML to JSON: ")
	msg = strings.TrimPrefix(msg, "error unmarshaling JSON: while decoding JSON: ")
	return fmt.Errorf("%s", msg)
}

// NodeIDs returns node ids in a stable order: start first, then BFS order, then the rest.
func (f *Flow) NodeIDs() []string {
	seen := map[string]bool{}
	var out []string
	queue := []string{f.Spec.Start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		n, ok := f.Spec.Nodes[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		for _, o := range n.Outcomes {
			queue = append(queue, n.Next[o])
		}
		var rest []string
		for o, t := range n.Next {
			if !contains(n.Outcomes, o) {
				rest = append(rest, t)
			}
		}
		sort.Strings(rest)
		queue = append(queue, rest...)
		if n.OnExhausted != "" {
			queue = append(queue, n.OnExhausted)
		}
	}
	var rest []string
	for id := range f.Spec.Nodes {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Terminal reports whether a transition target ends the run.
func Terminal(target string) bool { return target == Success || target == Fail }
