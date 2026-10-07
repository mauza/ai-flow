// Package eval scores captured GET /api/runs/{id} responses without contacting
// a server, opening the store, or executing a model. Its runtime is stdlib-only.
package eval

import "encoding/json"

const SchemaVersion = 1

type Suite struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Provenance    string `json:"provenance"` // synthetic or captured
	Cases         []Case `json:"cases"`
}

type Case struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Capture     string `json:"capture"` // relative to the suite file
	FlowName    string `json:"flow_name"`
	FlowVersion int    `json:"flow_version"`
	TaskID      string `json:"task_id,omitempty"`
	Expect      Rubric `json:"expect"`
}

type Rubric struct {
	Status              string              `json:"status"`
	MaxDurationMS       *int64              `json:"max_duration_ms,omitempty"`
	MaxCostUSD          *float64            `json:"max_cost_usd,omitempty"`
	MaxTokens           *int64              `json:"max_tokens,omitempty"`
	MaxFailedVisits     *int64              `json:"max_failed_visits,omitempty"`
	MaxHumanCorrections *int64              `json:"max_human_corrections,omitempty"`
	FailureOutcomes     map[string][]string `json:"failure_outcomes,omitempty"`
	CorrectionOutcomes  map[string][]string `json:"correction_outcomes,omitempty"`
	Nodes               []NodeExpectation   `json:"nodes,omitempty"`
}

type NodeExpectation struct {
	Node    string                     `json:"node"`
	Outcome string                     `json:"outcome,omitempty"`
	Outputs map[string]json.RawMessage `json:"outputs,omitempty"`
}

// Capture deliberately reads only public API fields. Unknown API fields are
// allowed; suite/report fields are strict to catch misspelled scoring rules.
type Capture struct {
	Run    *Run    `json:"run"`
	Visits []Visit `json:"visits"`
}

type Run struct {
	ID          string   `json:"id"`
	FlowName    string   `json:"flow_name"`
	FlowVersion int      `json:"flow_version"`
	TaskID      string   `json:"task_id"`
	Status      string   `json:"status"`
	Error       string   `json:"error"`
	CostUSD     *float64 `json:"cost_usd"`
	Tokens      *int64   `json:"tokens"`
	StartedAt   int64    `json:"started_at"`
	FinishedAt  int64    `json:"finished_at"`
}

type Visit struct {
	RunID     string                     `json:"run_id"`
	Seq       int                        `json:"seq"`
	Node      string                     `json:"node"`
	Type      string                     `json:"type"`
	Status    string                     `json:"status"`
	Outcome   string                     `json:"outcome"`
	Outputs   map[string]json.RawMessage `json:"outputs"`
	Error     string                     `json:"error"`
	DecidedBy string                     `json:"decided_by"`
}

type Metrics struct {
	DurationMS       *int64   `json:"duration_ms"`
	CostUSD          *float64 `json:"cost_usd"`
	Tokens           *int64   `json:"tokens"`
	FailedVisits     int64    `json:"failed_visits"`
	GateDecisions    int64    `json:"gate_decisions"`
	HumanCorrections *int64   `json:"human_corrections"`
}

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Failure struct {
	Seq     int    `json:"seq"`
	Status  string `json:"status"`
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
}

type NodeResult struct {
	Node     string    `json:"node"`
	Visits   int       `json:"visits"`
	Failures []Failure `json:"failures"`
}

type CaseResult struct {
	ID            string       `json:"id"`
	FlowName      string       `json:"flow_name"`
	FlowVersion   int          `json:"flow_version"`
	TaskID        string       `json:"task_id,omitempty"`
	RunID         string       `json:"run_id"`
	RunStatus     string       `json:"run_status"`
	RunError      string       `json:"run_error,omitempty"`
	CaptureSHA256 string       `json:"capture_sha256"`
	RubricSHA256  string       `json:"rubric_sha256"`
	Passed        bool         `json:"passed"`
	Metrics       Metrics      `json:"metrics"`
	Checks        []Check      `json:"checks"`
	Nodes         []NodeResult `json:"nodes"`
}

type Summary struct {
	Total          int     `json:"total"`
	Passed         int     `json:"passed"`
	RunSucceeded   int     `json:"run_succeeded"`
	EvalPassRate   float64 `json:"eval_pass_rate"`
	RunSuccessRate float64 `json:"run_success_rate"`
}

type Report struct {
	SchemaVersion int          `json:"schema_version"`
	Suite         string       `json:"suite"`
	Provenance    string       `json:"provenance"`
	Summary       Summary      `json:"summary"`
	Cases         []CaseResult `json:"cases"`
	Comparison    *Comparison  `json:"comparison,omitempty"`
}

type Comparison struct {
	BaselineSHA256 string      `json:"baseline_sha256"`
	Cases          []CaseDelta `json:"cases"`
}

type CaseDelta struct {
	ID                  string  `json:"id"`
	BaselineFlowVersion int     `json:"baseline_flow_version"`
	CurrentFlowVersion  int     `json:"current_flow_version"`
	BaselinePassed      bool    `json:"baseline_passed"`
	CurrentPassed       bool    `json:"current_passed"`
	Regression          bool    `json:"regression"`
	Delta               Metrics `json:"delta"` // current minus baseline; null if unknown
}
