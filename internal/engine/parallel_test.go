package engine_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

const parallelFlow = header + `  start: checks
  nodes:
    checks:
      type: parallel
      branches: [lint, test, review]
      join: gather
      max_visits: 2
    lint:
      type: check
      run: make lint
      next: { pass: gather, fail: gather }
    test:
      type: check
      run: make test
      next: { pass: gather, fail: diagnose }
    diagnose:
      type: agent
      model: ` + testModel + `
      prompt: Explain the failure.
      outcomes: [explained]
      next: { explained: gather }
    review:
      type: llm
      model: ` + testModel + `
      prompt: review
      outcomes: [approve, changes]
      next: { approve: gather, changes: gather }
    gather:
      type: join
      cases:
        - when: 'nodes.lint.outcome == "pass" && nodes.test.outcome == "pass" && nodes.review.outcome == "approve"'
          outcome: green
      default: red
      next: { green: $success, red: fix }
    fix:
      type: agent
      model: ` + testModel + `
      prompt: fix
      outcomes: [done]
      next: { done: checks }
`

// active returns the run's unfinished visits by node.
func (h *harness) active(runID string) map[string]*store.Visit {
	h.t.Helper()
	vs, err := h.st.ActiveVisits(h.ctx, runID)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]*store.Visit{}
	for _, v := range vs {
		out[v.Node] = v
	}
	return out
}

func (h *harness) activeNodes(runID string) string {
	var ns []string
	for n := range h.active(runID) {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return strings.Join(ns, ",")
}

// finishNode reports a result for node's unfinished visit, like the broker.
func (h *harness) finishNode(runID, node, outcome string) {
	h.t.Helper()
	v := h.active(runID)[node]
	if v == nil {
		h.t.Fatalf("%s is not running (active: %s; path %s)", node, h.activeNodes(runID), h.path(runID))
	}
	if err := h.st.UpdateVisit(h.ctx, runID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": outcome, "outputs": "{}", "finished_at": store.Now()}); err != nil {
		h.t.Fatal(err)
	}
	h.e.Tick(h.ctx)
}

func TestParallelBranchesJoinAndLoop(t *testing.T) {
	h := newHarness(t)
	id := h.flow(parallelFlow)
	if got := h.activeNodes(id); got != "checks,lint,review,test" {
		t.Fatalf("all branches should start at once: %s", got)
	}
	if r := h.run(id); r.Status != store.RunRunning || r.CurrentNode != "checks" {
		t.Fatalf("run %s at %s", r.Status, r.CurrentNode)
	}
	if len(h.l.launched) != 3 {
		t.Fatalf("launches %+v", h.l.launched)
	}

	h.finishNode(id, "lint", "pass")
	h.finishNode(id, "review", "approve")
	if got := h.activeNodes(id); got != "checks,test" {
		t.Fatalf("finished branches wait at the join: %s", got)
	}
	h.finishNode(id, "test", "fail") // a branch can take more than one step
	if got := h.activeNodes(id); got != "checks,diagnose" {
		t.Fatalf("test fail → diagnose inside its branch: %s", got)
	}
	h.finishNode(id, "diagnose", "explained") // last branch arrives → join → red → fix
	if r := h.run(id); r.CurrentNode != "fix" {
		t.Fatalf("join should route red to fix, at %s (%s)", r.CurrentNode, h.path(id))
	}

	h.finishNode(id, "fix", "done") // back to the parallel node: all branches again
	if got := h.activeNodes(id); got != "checks,lint,review,test" {
		t.Fatalf("second pass: %s", got)
	}
	for _, n := range []string{"test", "review", "lint"} {
		h.finishNode(id, n, map[string]string{"test": "pass", "review": "approve", "lint": "pass"}[n])
	}
	r := h.run(id)
	if r.Status != store.RunSucceeded {
		t.Fatalf("want success, got %s: %s (%s)", r.Status, r.Error, h.path(id))
	}

	vs, _ := h.st.Visits(h.ctx, id)
	forks := map[string]int{}
	for _, v := range vs {
		switch v.Node {
		case "checks":
			if v.ForkSeq != 0 || v.Outcome != "joined" {
				t.Errorf("parallel visit %+v", v)
			}
		case "gather", "fix":
			if v.ForkSeq != 0 {
				t.Errorf("%s is on the main path: %+v", v.Node, v)
			}
		default:
			if v.ForkSeq == 0 || v.Lane == "" {
				t.Errorf("%s should run in a branch: %+v", v.Node, v)
			}
			if v.Node == "diagnose" && v.Lane != "test" {
				t.Errorf("diagnose belongs to the test branch: %+v", v)
			}
			forks[v.Node+"@"+v.Lane]++
		}
	}
	if forks["lint@lint"] != 2 || forks["diagnose@test"] != 1 {
		t.Errorf("branch visits %v", forks)
	}
}

func TestParallelBranchErrorStopsTheOthers(t *testing.T) {
	h := newHarness(t)
	id := h.flow(parallelFlow)
	test := h.active(id)["test"]
	if err := h.st.UpdateVisit(h.ctx, id, test.Seq, map[string]any{"status": store.VisitError, "error": "node pod failed: OOMKilled", "finished_at": store.Now()}); err != nil {
		t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	r := h.run(id)
	if r.Status != store.RunFailed || !strings.Contains(r.Error, "test: node pod failed") {
		t.Fatalf("got %s: %q", r.Status, r.Error)
	}
	if got := h.activeNodes(id); got != "" {
		t.Errorf("nothing may stay active: %s", got)
	}
	for _, s := range h.l.launched {
		if st := h.l.state[engine.JobName(s)]; st != engine.JobFailed {
			t.Errorf("job for %s still %s", s.Node, st)
		}
	}

	// Resume defaults to the parallel node and reruns every branch.
	if _, err := h.e.Resume(h.ctx, id, "test", "", "t"); err == nil || !strings.Contains(err.Error(), `resume at "checks"`) {
		t.Fatalf("resuming inside a branch: %v", err)
	}
	if _, err := h.e.Resume(h.ctx, id, "", "", "t"); err != nil {
		t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	if got := h.activeNodes(id); got != "checks,lint,review,test" {
		t.Fatalf("resume should rerun all branches: %s", got)
	}
}

func TestParallelCancelStopsEveryBranch(t *testing.T) {
	h := newHarness(t)
	id := h.flow(parallelFlow)
	if err := h.e.Cancel(h.ctx, id); err != nil {
		t.Fatal(err)
	}
	if r := h.run(id); r.Status != store.RunCanceled {
		t.Fatalf("status %s", r.Status)
	}
	for _, s := range h.l.launched {
		if st := h.l.state[engine.JobName(s)]; st != engine.JobFailed {
			t.Errorf("job for %s still %s", s.Node, st)
		}
	}
}

func TestBranchContextShowsOnlyItsOwnBranch(t *testing.T) {
	h := newHarness(t)
	id := h.flow(parallelFlow)
	h.finishNode(id, "lint", "pass")
	h.finishNode(id, "review", "approve")
	h.finishNode(id, "test", "fail")
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	diag := h.active(id)["diagnose"]
	md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["diagnose"], diag.Seq)
	if !strings.Contains(md, "## Previous step: test → fail") || strings.Contains(md, "lint#1") || strings.Contains(md, "review#1") {
		t.Errorf("branch context:\n%s", md)
	}
	// The join's CEL sees every branch.
	c := h.e.TemplateContext(h.ctx, r, res, res.Nodes["gather"])
	b, _ := json.Marshal(c["nodes"])
	for _, n := range []string{`"lint"`, `"review"`, `"test"`} {
		if !strings.Contains(string(b), n) {
			t.Errorf("join context lacks %s: %s", n, b)
		}
	}
}
