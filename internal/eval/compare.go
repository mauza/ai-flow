package eval

import (
	"fmt"
	"os"
)

// CompareFile pairs cases by stable ID, not position. Versions may change, but
// suite, provenance, flow names, case set and scoring rules must match.
func CompareFile(current *Report, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var baseline Report
	if err := decode(data, &baseline, true); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if baseline.SchemaVersion != SchemaVersion || baseline.Suite != current.Suite || baseline.Provenance != current.Provenance || len(baseline.Cases) != len(current.Cases) {
		return fmt.Errorf("baseline must match schema, suite, provenance and case set")
	}
	byID := map[string]CaseResult{}
	for _, c := range baseline.Cases {
		if _, exists := byID[c.ID]; exists || c.ID == "" {
			return fmt.Errorf("baseline has empty or duplicate case ID %q", c.ID)
		}
		if c.FlowVersion <= 0 || c.RunID == "" || !terminal(c.RunStatus) || len(c.CaptureSHA256) != 64 || len(c.RubricSHA256) != 64 || len(c.Checks) == 0 {
			return fmt.Errorf("baseline case %s is incomplete", c.ID)
		}
		passed := true
		for _, check := range c.Checks {
			passed = passed && check.Passed
		}
		if passed != c.Passed {
			return fmt.Errorf("baseline case %s passed flag contradicts checks", c.ID)
		}
		byID[c.ID] = c
	}
	comparison := &Comparison{BaselineSHA256: digest(data), Cases: []CaseDelta{}}
	for _, c := range current.Cases {
		b, ok := byID[c.ID]
		if !ok || b.FlowName != c.FlowName || b.RubricSHA256 != c.RubricSHA256 {
			return fmt.Errorf("baseline case %s missing or has different flow/scoring rules", c.ID)
		}
		comparison.Cases = append(comparison.Cases, CaseDelta{
			ID: c.ID, BaselineFlowVersion: b.FlowVersion, CurrentFlowVersion: c.FlowVersion,
			BaselinePassed: b.Passed, CurrentPassed: c.Passed, Regression: b.Passed && !c.Passed,
			Delta: Metrics{
				DurationMS:       subtract(c.Metrics.DurationMS, b.Metrics.DurationMS),
				CostUSD:          subtract(c.Metrics.CostUSD, b.Metrics.CostUSD),
				Tokens:           subtract(c.Metrics.Tokens, b.Metrics.Tokens),
				FailedVisits:     c.Metrics.FailedVisits - b.Metrics.FailedVisits,
				GateDecisions:    c.Metrics.GateDecisions - b.Metrics.GateDecisions,
				HumanCorrections: subtract(c.Metrics.HumanCorrections, b.Metrics.HumanCorrections),
			},
		})
	}
	current.Comparison = comparison
	return nil
}

func subtract[T int64 | float64](a, b *T) *T {
	if a == nil || b == nil {
		return nil
	}
	delta := *a - *b
	return &delta
}
