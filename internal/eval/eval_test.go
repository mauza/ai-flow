package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/store"
)

func ptr[T any](v T) *T { return &v }

func testCase() Case {
	return Case{ID: "test", Capture: "capture.json", FlowName: "flow", FlowVersion: 1,
		Expect: Rubric{Status: "succeeded", Nodes: []NodeExpectation{{Node: "verify", Outcome: "pass", Outputs: map[string]json.RawMessage{"exit_code": json.RawMessage(`0`)}}}}}
}

func testCapture() Capture {
	return Capture{
		Run:    &Run{ID: "run", FlowName: "flow", FlowVersion: 1, Status: "succeeded", StartedAt: 1000, FinishedAt: 3000, CostUSD: ptr(0.25), Tokens: ptr(int64(42))},
		Visits: []Visit{{RunID: "run", Seq: 1, Node: "verify", Type: "check", Status: "succeeded", Outcome: "pass", Outputs: map[string]json.RawMessage{"exit_code": json.RawMessage(`0`)}}},
	}
}

func raw(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.WriteFile(path, raw(t, v), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestExamplesAndComparison(t *testing.T) {
	baseline, err := EvaluateFile("../../examples/evals/baseline-suite.json")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Summary.Passed != 2 || baseline.Summary.RunSucceeded != 1 || baseline.Summary.EvalPassRate != 1 || baseline.Summary.RunSuccessRate != 0.5 {
		t.Fatalf("evaluation must differ from run status: %+v", baseline.Summary)
	}
	if baseline.Cases[0].Metrics.HumanCorrections != nil || *baseline.Cases[1].Metrics.HumanCorrections != 1 || baseline.Cases[1].Metrics.GateDecisions != 2 {
		t.Fatal("human corrections must require explicit outcome mapping")
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	// Ordering is not the comparison key.
	baseline.Cases[0], baseline.Cases[1] = baseline.Cases[1], baseline.Cases[0]
	writeJSON(t, path, baseline)
	candidate, err := EvaluateFile("../../examples/evals/candidate-suite.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareFile(candidate, path); err != nil {
		t.Fatal(err)
	}
	delta := candidate.Comparison.Cases[0]
	if candidate.Summary.Passed != 1 || !delta.Regression || *delta.Delta.DurationMS != 10000 || *delta.Delta.Tokens != 500 || delta.Delta.FailedVisits != 2 || delta.BaselineFlowVersion != 1 || delta.CurrentFlowVersion != 2 {
		t.Fatalf("unexpected regression report: %+v", candidate)
	}
	if delta.Delta.HumanCorrections != nil {
		t.Fatal("unknown must remain unknown in comparisons")
	}
	// Same bytes and rules produce byte-identical reports: no wall clock fields.
	again, err := EvaluateFile("../../examples/evals/candidate-suite.json")
	if err != nil {
		t.Fatal(err)
	}
	candidate.Comparison = nil
	if string(raw(t, again)) != string(raw(t, candidate)) {
		t.Fatal("report is nondeterministic")
	}
}

func TestLatestVisitAndRecoveredFailures(t *testing.T) {
	c := testCase()
	c.Expect.MaxFailedVisits = ptr(int64(1))
	c.Expect.FailureOutcomes = map[string][]string{"verify": {"fail"}}
	capture := testCapture()
	// An earlier failing check is still a successfully executed node. Reversed
	// array order exercises selection by seq, not by slice order or first match.
	capture.Visits[0].Seq = 2
	capture.Visits = append(capture.Visits, Visit{RunID: "run", Seq: 1, Node: "verify", Type: "check", Status: "succeeded", Outcome: "fail"})
	r, err := score(c, raw(t, capture))
	if err != nil || !r.Passed || r.Metrics.FailedVisits != 1 || r.Nodes[0].Failures[0].Seq != 1 {
		t.Fatalf("recovered run: %+v %v", r, err)
	}
	capture.Visits[0].Status = "error"
	capture.Visits[0].Error = "later retry broke"
	r, err = score(c, raw(t, capture))
	if err != nil || r.Passed || r.Metrics.FailedVisits != 2 {
		t.Fatalf("must not use an older successful visit: %+v %v", r, err)
	}
}

func TestUnknownMetricsFailThresholds(t *testing.T) {
	c := testCase()
	c.Expect.MaxDurationMS, c.Expect.MaxTokens, c.Expect.MaxCostUSD = ptr(int64(5000)), ptr(int64(100)), ptr(1.0)
	capture := testCapture()
	capture.Run.StartedAt, capture.Run.FinishedAt = 0, 0
	capture.Run.CostUSD, capture.Run.Tokens = nil, nil
	r, err := score(c, raw(t, capture))
	if err != nil || r.Passed || r.Metrics.DurationMS != nil || r.Metrics.Tokens != nil || r.Metrics.CostUSD != nil {
		t.Fatalf("missing metrics must not become zero: %+v %v", r, err)
	}
	unknown := 0
	for _, check := range r.Checks {
		if !check.Passed && check.Detail == "metric unavailable" {
			unknown++
		}
	}
	if unknown != 3 {
		t.Fatalf("want 3 unavailable metric checks, got %d", unknown)
	}
}

func TestHumanCorrectionsExcludeTimeoutAndAutomation(t *testing.T) {
	c := testCase()
	c.Expect.CorrectionOutcomes = map[string][]string{"approve": {"revise", "timeout"}, "verify": {"pass"}}
	c.Expect.MaxHumanCorrections = ptr(int64(1))
	capture := testCapture()
	capture.Visits[0].DecidedBy = "not-a-gate"
	for i, by := range []string{"reviewer", "timeout", ""} {
		capture.Visits = append(capture.Visits, Visit{RunID: "run", Seq: i + 2, Node: "approve", Type: "gate", Status: "succeeded", Outcome: "revise", DecidedBy: by})
	}
	r, err := score(c, raw(t, capture))
	if err != nil || !r.Passed || *r.Metrics.HumanCorrections != 1 || r.Metrics.GateDecisions != 1 {
		t.Fatalf("only explicit recorded gate decisions count: %+v %v", r, err)
	}
}

func TestRejectInvalidCaptures(t *testing.T) {
	mutations := map[string]func(*Capture){
		"missing run":      func(c *Capture) { c.Run = nil },
		"missing visits":   func(c *Capture) { c.Visits = nil },
		"missing type":     func(c *Capture) { c.Visits[0].Type = "" },
		"unpinned version": func(c *Capture) { c.Run.FlowVersion = 0 },
		"wrong version":    func(c *Capture) { c.Run.FlowVersion = 2 },
		"wrong flow":       func(c *Capture) { c.Run.FlowName = "other" },
		"not terminal":     func(c *Capture) { c.Run.Status = "waiting" },
		"unfinished visit": func(c *Capture) { c.Visits[0].Status = "running" },
		"duplicate seq":    func(c *Capture) { c.Visits = append(c.Visits, c.Visits[0]) },
		"foreign visit":    func(c *Capture) { c.Visits[0].RunID = "other" },
		"reversed time":    func(c *Capture) { c.Run.FinishedAt = 999 },
		"negative cost":    func(c *Capture) { c.Run.CostUSD = ptr(-1.0) },
		"negative tokens":  func(c *Capture) { c.Run.Tokens = ptr(int64(-1)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			capture := testCapture()
			mutate(&capture)
			if _, err := score(testCase(), raw(t, capture)); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	c := testCase()
	c.TaskID = "wanted-task"
	if _, err := score(c, raw(t, testCapture())); err == nil {
		t.Fatal("accepted mismatched task")
	}
}

func TestSuiteValidationAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "capture.json"), testCapture())
	suite := Suite{SchemaVersion: 1, Name: "test", Provenance: "synthetic", Cases: []Case{testCase()}}
	path := filepath.Join(dir, "suite.json")
	writeJSON(t, path, suite)
	if r, err := EvaluateFile(path); err != nil || r.Summary.Passed != 1 {
		t.Fatalf("relative capture path: %+v %v", r, err)
	}
	for name, mutate := range map[string]func(*Suite){
		"schema":                  func(s *Suite) { s.SchemaVersion = 99 },
		"provenance":              func(s *Suite) { s.Provenance = "" },
		"empty":                   func(s *Suite) { s.Cases = nil },
		"duplicate":               func(s *Suite) { s.Cases = append(s.Cases, s.Cases[0]) },
		"version":                 func(s *Suite) { s.Cases[0].FlowVersion = 0 },
		"status":                  func(s *Suite) { s.Cases[0].Expect.Status = "" },
		"negative threshold":      func(s *Suite) { s.Cases[0].Expect.MaxTokens = ptr(int64(-1)) },
		"unmeasurable correction": func(s *Suite) { s.Cases[0].Expect.MaxHumanCorrections = ptr(int64(0)) },
		"empty assertion":         func(s *Suite) { s.Cases[0].Expect.Nodes = []NodeExpectation{{Node: "verify"}} },
	} {
		t.Run(name, func(t *testing.T) {
			s := Suite{SchemaVersion: 1, Name: "test", Provenance: "synthetic", Cases: []Case{testCase()}}
			mutate(&s)
			if err := validateSuite(s); err == nil {
				t.Fatal("accepted invalid suite")
			}
		})
	}
	for _, data := range []string{strings.Replace(string(raw(t, suite)), `"expect":`, `"exepct":`, 1), string(raw(t, suite)) + `{}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := EvaluateFile(path); err == nil {
			t.Fatal("accepted misspelled rule or trailing JSON")
		}
	}
}

func TestComparisonRejectsIncompatibleBaseline(t *testing.T) {
	for name, mutate := range map[string]func(*Report){
		"schema":      func(r *Report) { r.SchemaVersion = 99 },
		"suite":       func(r *Report) { r.Suite = "other" },
		"provenance":  func(r *Report) { r.Provenance = "captured" },
		"case set":    func(r *Report) { r.Cases = r.Cases[:1] },
		"case ID":     func(r *Report) { r.Cases[0].ID = "other" },
		"duplicate":   func(r *Report) { r.Cases[1].ID = r.Cases[0].ID },
		"flow":        func(r *Report) { r.Cases[0].FlowName = "other" },
		"rubric":      func(r *Report) { r.Cases[0].RubricSHA256 = strings.Repeat("0", 64) },
		"passed flag": func(r *Report) { r.Cases[0].Passed = false },
	} {
		t.Run(name, func(t *testing.T) {
			current, err := EvaluateFile("../../examples/evals/baseline-suite.json")
			if err != nil {
				t.Fatal(err)
			}
			var baseline Report
			if err := decode(raw(t, current), &baseline, true); err != nil {
				t.Fatal(err)
			}
			mutate(&baseline)
			path := filepath.Join(t.TempDir(), "baseline.json")
			writeJSON(t, path, baseline)
			if err := CompareFile(current, path); err == nil {
				t.Fatal("accepted incompatible baseline")
			}
		})
	}
}

func TestJSONEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{`{"a":[1,true,null],"b":2}`, `{"b":2.0,"a":[1.0,true,null]}`, true},
		{`9007199254740992`, `9007199254740993`, false},
		{`null`, `false`, false},
		{`{"a":null}`, `{"b":null}`, false},
		{`[1,2]`, `[2,1]`, false},
	} {
		if got := jsonEqual([]byte(tc.a), []byte(tc.b)); got != tc.equal {
			t.Errorf("%s == %s: got %v", tc.a, tc.b, got)
		}
	}
	c := testCase()
	c.Expect.Nodes[0].Outputs = map[string]json.RawMessage{"missing": json.RawMessage(`null`)}
	r, err := score(c, raw(t, testCapture()))
	if err != nil || r.Passed {
		t.Fatalf("absent field must not equal null: %+v %v", r, err)
	}
}

func TestStoreExportCompatibility(t *testing.T) {
	// The server encodes these store types directly inside the run response.
	// This guards the offline reader contract without opening SQLite.
	data := raw(t, map[string]any{
		"run":    store.Run{ID: "run", FlowName: "flow", FlowVersion: 1, Status: store.RunSucceeded, Tokens: 42, CostUSD: .25, StartedAt: 1000, FinishedAt: 3000},
		"visits": []store.Visit{{RunID: "run", Seq: 1, Node: "verify", Type: "check", Status: store.VisitSucceeded, Outcome: "pass", Outputs: json.RawMessage(`{"exit_code":0}`)}},
		"events": []any{}, "yaml": "ignored API field",
	})
	r, err := score(testCase(), data)
	if err != nil || !r.Passed || *r.Metrics.Tokens != 42 || *r.Metrics.DurationMS != 2000 {
		t.Fatalf("public store export mismatch: %+v %v", r, err)
	}
}
