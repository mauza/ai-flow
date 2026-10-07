package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/protocol"
)

func TestCheckStructuredOutputs(t *testing.T) {
	declared := map[string]any{"passed": "integer", "failed": "integer", "coverage": "number", "failing": []any{"string"}, "exit_code": "integer"}
	cases := []struct {
		name, run, outcome, err string
		want                    map[string]any
	}{
		{name: "written", run: `echo ok; printf '{"passed":41,"failed":1,"coverage":87.5,"failing":["TestSlug"]}' > "$AI_FLOW_OUTPUTS"; exit 1`,
			outcome: "fail", want: map[string]any{"passed": 41.0, "failed": 1.0, "coverage": 87.5, "failing": []any{"TestSlug"}, "exit_code": 1}},
		{name: "partial", run: `printf '{"passed":3}' > "$AI_FLOW_OUTPUTS"`, outcome: "pass", want: map[string]any{"passed": 3.0, "exit_code": 0}},
		{name: "not written", run: `exit 2`, outcome: "fail", want: map[string]any{"exit_code": 2}},
		{name: "not json", run: `echo nope > "$AI_FLOW_OUTPUTS"`, err: "one JSON object"},
		{name: "a list", run: `echo '[1]' > "$AI_FLOW_OUTPUTS"`, err: "one JSON object"},
		{name: "undeclared", run: `echo '{"surprise":1}' > "$AI_FLOW_OUTPUTS"`, err: `"surprise" is not declared`},
		{name: "reserved", run: `echo '{"exit_code":0}' > "$AI_FLOW_OUTPUTS"`, err: `"exit_code" is set by the runner`},
		{name: "wrong type", run: `echo '{"passed":"41"}' > "$AI_FLOW_OUTPUTS"`, err: "passed: want an integer, got a string"},
		{name: "not integer", run: `echo '{"failed":1.5}' > "$AI_FLOW_OUTPUTS"`, err: "want an integer"},
		{name: "bad item", run: `echo '{"failing":["a",2]}' > "$AI_FLOW_OUTPUTS"`, err: "failing: [1]: want a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runtimeRunner(t)
			r.b.Outputs = declared
			r.b.Check = &protocol.CheckSpec{Run: c.run, ExitCodes: map[string]string{"0": "pass", "default": "fail"}}
			res := r.runCheck(context.Background())
			if c.err != "" {
				if res.Error == "" || !strings.Contains(res.Error, c.err) || res.Outcome != "" {
					t.Fatalf("want error %q, got %+v", c.err, res)
				}
				return
			}
			if res.Error != "" || res.Outcome != c.outcome {
				t.Fatalf("result %+v", res)
			}
			if _, ok := res.Outputs["log_tail"].(string); !ok {
				t.Errorf("log_tail missing: %+v", res.Outputs)
			}
			delete(res.Outputs, "log_tail")
			if got, want := toJSON(t, res.Outputs), toJSON(t, c.want); got != want {
				t.Errorf("outputs %s, want %s", got, want)
			}
		})
	}
}

func TestCheckOutputsFileIsFreshEachRun(t *testing.T) {
	r := runtimeRunner(t)
	r.b.Outputs = map[string]any{"n": "integer"}
	r.b.Check = &protocol.CheckSpec{Run: `printf '{"n":1}' > "$AI_FLOW_OUTPUTS"`, ExitCodes: map[string]string{"0": "pass", "default": "fail"}}
	if res := r.runCheck(context.Background()); res.Outputs["n"] != 1.0 {
		t.Fatalf("first run %+v", res)
	}
	r.b.Check.Run = `true`
	if res := r.runCheck(context.Background()); res.Outputs["n"] != nil {
		t.Fatalf("a stale outputs file leaked into the next run: %+v", res.Outputs)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
