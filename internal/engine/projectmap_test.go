package engine_test

import (
	"strings"
	"testing"
)

const mapFlow = header + `  start: explore
  nodes:
    explore:
      type: agent
      model: ` + testModel + `
      prompt: explore
      outcomes: [mapped]
      outputs: { project_map: string, map_commit: string }
      next: { mapped: build }
    build:
      type: agent
      model: ` + testModel + `
      prompt: build it
      outcomes: [done]
      next: { done: $success }
`

// What one exploration learns is the starting point of later steps and runs.
func TestProjectMapCarriesAcrossStepsAndRuns(t *testing.T) {
	h := newHarness(t)
	id := h.flow(mapFlow)
	ctxOf := func(id, node string) string {
		v, _ := h.st.LastVisit(h.ctx, id)
		r := h.run(id)
		res, err := h.e.Resolved(h.ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return h.e.ContextMarkdown(h.ctx, r, res, res.Nodes[node], v.Seq)
	}
	if first := ctxOf(id, "explore"); strings.Contains(first, "## Project map") || !strings.Contains(first, "## Updating the project map") {
		t.Fatalf("first exploration:\n%s", first)
	}
	h.finish(id, "mapped", map[string]any{"project_map": "## Modules\n- server/: Go API, accounts in accounts.go", "map_commit": "abc1234"})
	got := ctxOf(id, "build")
	if !strings.Contains(got, "## Project map") || !strings.Contains(got, "accounts in accounts.go") || !strings.Contains(got, "git diff --stat abc1234..HEAD") {
		t.Fatalf("later step:\n%s", got)
	}
	if strings.Contains(got, "## Updating the project map") {
		t.Fatal("a step without the output is not asked to update the map")
	}
	h.finish(id, "done", nil)

	next := h.flow(mapFlow) // another run of the project
	if got := ctxOf(next, "explore"); !strings.Contains(got, "accounts in accounts.go") {
		t.Fatalf("next run:\n%s", got)
	}
}
