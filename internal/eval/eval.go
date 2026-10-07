package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
)

func decode(data []byte, dst any, strict bool) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document")
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func terminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled"
}

// EvaluateFile reads a suite and its captures. Invalid/incomplete input returns
// an error; valid terminal runs that miss expectations return failed checks.
func EvaluateFile(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var suite Suite
	if err := decode(data, &suite, true); err != nil {
		return nil, fmt.Errorf("suite: %w", err)
	}
	if err := validateSuite(suite); err != nil {
		return nil, err
	}
	report := &Report{SchemaVersion: SchemaVersion, Suite: suite.Name, Provenance: suite.Provenance}
	for _, c := range suite.Cases {
		capturePath := c.Capture
		if !filepath.IsAbs(capturePath) {
			capturePath = filepath.Join(filepath.Dir(path), capturePath)
		}
		data, err := os.ReadFile(capturePath)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		result, err := score(c, data)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		report.Cases = append(report.Cases, result)
		if result.Passed {
			report.Summary.Passed++
		}
		if result.RunStatus == "succeeded" {
			report.Summary.RunSucceeded++
		}
	}
	report.Summary.Total = len(report.Cases)
	report.Summary.EvalPassRate = float64(report.Summary.Passed) / float64(report.Summary.Total)
	report.Summary.RunSuccessRate = float64(report.Summary.RunSucceeded) / float64(report.Summary.Total)
	return report, nil
}

func validateSuite(s Suite) error {
	if s.SchemaVersion != SchemaVersion || s.Name == "" || len(s.Cases) == 0 {
		return fmt.Errorf("suite needs schema_version %d, name and nonempty cases", SchemaVersion)
	}
	if s.Provenance != "synthetic" && s.Provenance != "captured" {
		return fmt.Errorf("provenance must be synthetic or captured")
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if c.ID == "" || seen[c.ID] || c.Capture == "" || c.FlowName == "" || c.FlowVersion <= 0 {
			return fmt.Errorf("case %q needs unique id, capture, flow_name and positive pinned flow_version", c.ID)
		}
		seen[c.ID] = true
		r := c.Expect
		if !terminal(r.Status) {
			return fmt.Errorf("case %s: expect.status must be succeeded, failed or canceled", c.ID)
		}
		for _, n := range []*int64{r.MaxDurationMS, r.MaxTokens, r.MaxFailedVisits, r.MaxHumanCorrections} {
			if n != nil && *n < 0 {
				return fmt.Errorf("case %s: limits must be nonnegative", c.ID)
			}
		}
		if r.MaxCostUSD != nil && *r.MaxCostUSD < 0 {
			return fmt.Errorf("case %s: max_cost_usd must be nonnegative", c.ID)
		}
		if r.MaxHumanCorrections != nil && len(r.CorrectionOutcomes) == 0 {
			return fmt.Errorf("case %s: max_human_corrections requires correction_outcomes", c.ID)
		}
		for _, mapping := range []map[string][]string{r.FailureOutcomes, r.CorrectionOutcomes} {
			for node, outcomes := range mapping {
				if node == "" || len(outcomes) == 0 || slices.Contains(outcomes, "") {
					return fmt.Errorf("case %s: outcome mappings need a node and nonempty outcomes", c.ID)
				}
			}
		}
		seenNodes := map[string]bool{}
		for _, n := range r.Nodes {
			if n.Node == "" || seenNodes[n.Node] || (n.Outcome == "" && len(n.Outputs) == 0) {
				return fmt.Errorf("case %s: node expectations need unique node and outcome or outputs", c.ID)
			}
			seenNodes[n.Node] = true
		}
	}
	return nil
}

func score(c Case, data []byte) (CaseResult, error) {
	var capture Capture
	if err := decode(data, &capture, false); err != nil {
		return CaseResult{}, err
	}
	if err := validateCapture(capture); err != nil {
		return CaseResult{}, err
	}
	run := capture.Run
	if run.FlowName != c.FlowName || run.FlowVersion != c.FlowVersion || (c.TaskID != "" && run.TaskID != c.TaskID) {
		return CaseResult{}, fmt.Errorf("capture identity does not match pinned flow/task")
	}
	rubric, _ := json.Marshal(c.Expect)
	r := CaseResult{ID: c.ID, FlowName: run.FlowName, FlowVersion: run.FlowVersion, TaskID: run.TaskID,
		RunID: run.ID, RunStatus: run.Status, RunError: run.Error, CaptureSHA256: digest(data),
		RubricSHA256: digest(rubric), Passed: true, Nodes: []NodeResult{},
		Metrics: Metrics{CostUSD: run.CostUSD, Tokens: run.Tokens}}
	if run.StartedAt > 0 && run.FinishedAt >= run.StartedAt {
		duration := run.FinishedAt - run.StartedAt
		r.Metrics.DurationMS = &duration
	}
	if len(c.Expect.CorrectionOutcomes) > 0 {
		r.Metrics.HumanCorrections = new(int64)
	}
	latest := map[string]Visit{}
	nodes := map[string]*NodeResult{}
	sort.Slice(capture.Visits, func(i, j int) bool { return capture.Visits[i].Seq < capture.Visits[j].Seq })
	for _, v := range capture.Visits {
		latest[v.Node] = v
		if nodes[v.Node] == nil {
			nodes[v.Node] = &NodeResult{Node: v.Node, Failures: []Failure{}}
		}
		n := nodes[v.Node]
		n.Visits++
		if v.Status == "error" || v.Status == "canceled" || v.Error != "" || slices.Contains(c.Expect.FailureOutcomes[v.Node], v.Outcome) {
			n.Failures = append(n.Failures, Failure{Seq: v.Seq, Status: v.Status, Outcome: v.Outcome, Error: v.Error})
			r.Metrics.FailedVisits++
		}
		if v.Type == "gate" && v.Status == "succeeded" && v.DecidedBy != "" && v.DecidedBy != "timeout" {
			r.Metrics.GateDecisions++
			if slices.Contains(c.Expect.CorrectionOutcomes[v.Node], v.Outcome) {
				(*r.Metrics.HumanCorrections)++
			}
		}
	}
	for _, n := range nodes {
		r.Nodes = append(r.Nodes, *n)
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Node < r.Nodes[j].Node })
	add := func(name string, passed bool, detail string) {
		r.Checks = append(r.Checks, Check{Name: name, Passed: passed, Detail: detail})
		r.Passed = r.Passed && passed
	}
	add("run.status", run.Status == c.Expect.Status, fmt.Sprintf("want %s; got %s", c.Expect.Status, run.Status))
	checkLimit(add, "duration_ms", r.Metrics.DurationMS, c.Expect.MaxDurationMS)
	checkLimit(add, "cost_usd", r.Metrics.CostUSD, c.Expect.MaxCostUSD)
	checkLimit(add, "tokens", r.Metrics.Tokens, c.Expect.MaxTokens)
	checkLimit(add, "failed_visits", &r.Metrics.FailedVisits, c.Expect.MaxFailedVisits)
	checkLimit(add, "human_corrections", r.Metrics.HumanCorrections, c.Expect.MaxHumanCorrections)
	for _, want := range c.Expect.Nodes {
		v, ok := latest[want.Node]
		prefix := "node." + want.Node
		add(prefix+".completed", ok && v.Status == "succeeded", "latest visit must exist and have status succeeded")
		if want.Outcome != "" {
			add(prefix+".outcome", ok && v.Outcome == want.Outcome, fmt.Sprintf("want %s; got %s", want.Outcome, v.Outcome))
		}
		keys := make([]string, 0, len(want.Outputs))
		for key := range want.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			actual, exists := v.Outputs[key]
			add(prefix+".outputs."+key, exists && jsonEqual(actual, want.Outputs[key]), fmt.Sprintf("want %s; got %s", want.Outputs[key], actual))
		}
	}
	return r, nil
}

func validateCapture(c Capture) error {
	r := c.Run
	if r == nil || r.ID == "" || r.FlowName == "" || r.FlowVersion <= 0 || c.Visits == nil {
		return fmt.Errorf("capture needs run identity and visits array (empty array is allowed)")
	}
	if !terminal(r.Status) {
		return fmt.Errorf("run %s is not terminal: %s", r.ID, r.Status)
	}
	if r.StartedAt < 0 || r.FinishedAt < 0 || (r.StartedAt > 0 && r.FinishedAt > 0 && r.FinishedAt < r.StartedAt) ||
		(r.CostUSD != nil && *r.CostUSD < 0) || (r.Tokens != nil && *r.Tokens < 0) {
		return fmt.Errorf("invalid negative metrics or reversed timestamps")
	}
	seen := map[int]bool{}
	for _, v := range c.Visits {
		if v.RunID != r.ID || v.Node == "" || v.Type == "" || v.Seq <= 0 || seen[v.Seq] {
			return fmt.Errorf("visit must match run_id and have node, type and unique positive seq")
		}
		seen[v.Seq] = true
		if v.Status != "succeeded" && v.Status != "error" && v.Status != "canceled" {
			return fmt.Errorf("visit %d is not terminal: %s", v.Seq, v.Status)
		}
	}
	return nil
}

func checkLimit[T int64 | float64](add func(string, bool, string), name string, actual, limit *T) {
	if limit == nil {
		return
	}
	if actual == nil {
		add(name, false, "metric unavailable")
		return
	}
	add(name, *actual <= *limit, fmt.Sprintf("max %v; got %v", *limit, *actual))
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if decode(a, &x, false) != nil || decode(b, &y, false) != nil {
		return false
	}
	return equalValue(x, y)
}

func equalValue(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xr, xok := new(big.Rat).SetString(string(x))
		yr, yok := new(big.Rat).SetString(string(y))
		return xok && yok && xr.Cmp(yr) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equalValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalValue(x[i], y[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}
