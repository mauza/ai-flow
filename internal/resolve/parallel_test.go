package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
)

func validateYAML(t *testing.T, y string) (*Resolved, []string) {
	t.Helper()
	cfg := loadCfg(t)
	f, err := flow.Parse([]byte("apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: { name: par, project: sandbox }\nspec:\n" + y))
	if err != nil {
		t.Fatal(err)
	}
	r := Resolve(f, cfg)
	var out []string
	for _, i := range Validate(r, cfg) {
		out = append(out, i.String())
	}
	return r, out
}

const goodParallel = `  start: checks
  nodes:
    checks:
      type: parallel
      branches: [lint, test, review]
      join: gather
    lint:
      type: check
      run: make lint
      next: { pass: gather, fail: gather }
    test:
      type: check
      run: make test
      max_visits: 2
      next: { pass: gather, fail: diagnose }
    diagnose:
      type: agent
      model: gpt-6.1-sol
      prompt: Explain the failure; do not edit.
      outcomes: [explained]
      next: { explained: gather }
    review:
      type: llm
      model: gpt-6-luna
      prompt: Review the diff.
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
      model: gpt-6.1-sol
      prompt: Fix what the branches found.
      grants: [repo/ai-flow-sandbox:write]
      outcomes: [done]
      max_visits: 2
      next: { done: checks }
`

func TestParallelFlowValidatesAndProjects(t *testing.T) {
	r, issues := validateYAML(t, goodParallel)
	for _, i := range issues {
		t.Errorf("unexpected: %s", i)
	}
	if got := r.Nodes["checks"].Next; got[flow.OutcomeJoined] != "gather" || len(r.Nodes["checks"].Outcomes) != 1 {
		t.Errorf("parallel routing: %v %v", got, r.Nodes["checks"].Outcomes)
	}
	lanes, shared := r.Lanes()
	if len(shared) != 0 || lanes["diagnose"].Branch != "test" || lanes["review"].Fork != "checks" || lanes["gather"] != (Lane{}) || lanes["fix"] != (Lane{}) {
		t.Errorf("lanes %+v shared %v", lanes, shared)
	}
	g := r.Graph()
	var branches []string
	for _, e := range g.Edges {
		if e.From == "checks" {
			if e.Kind != "branch" {
				t.Errorf("parallel edge %+v", e)
			}
			branches = append(branches, e.To)
		}
	}
	if strings.Join(branches, ",") != "lint,test,review" {
		t.Errorf("branch edges %v", branches)
	}
}

func TestParallelValidationErrors(t *testing.T) {
	_, issues := validateYAML(t, `  start: checks
  nodes:
    checks:
      type: parallel
      branches: [lint, fix, lint]
      join: lint_out
      next: { joined: $success }
    lint:
      type: check
      run: make lint
      next: { pass: shared, fail: $success }
    fix:
      type: agent
      model: gpt-6.1-sol
      prompt: fix
      grants: [repo/ai-flow-sandbox:write]
      outcomes: [done, ask]
      next: { done: shared, ask: ask_human }
    shared:
      type: check
      run: make test
      next: { pass: lint_out, fail: lint_out }
    ask_human:
      type: gate
      prompt: help?
      outcomes: [ok]
      next: { ok: lint_out }
    lint_out:
      type: switch
      cases: [{ when: "true", outcome: done }]
      default: done
      next: { done: after }
    after:
      type: check
      run: "true"
      next: { pass: lint, fail: $fail }
`)
	all := strings.Join(issues, "\n")
	for _, want := range []string{
		"node checks.next: a parallel node continues at its join",
		`node checks.branches: duplicate branch "lint"`,
		`node checks.join: "lint_out" is a switch node, not a join`,
		"node shared: reached from more than one parallel branch",
		"node fix.grants: parallel branches are read-only",
		`node ask_human.type: gate nodes cannot run inside a parallel branch`,
		"node lint.next: a branch cannot end the run with $success",
		`node after.next: "lint" is inside branch "lint" of "checks"; only that branch may route to it`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestJoinOnlyFromItsBranches(t *testing.T) {
	_, issues := validateYAML(t, `  start: prep
  nodes:
    prep:
      type: check
      run: "true"
      next: { pass: checks, fail: gather }
    checks:
      type: parallel
      branches: [a, b]
      join: gather
    a: { type: check, run: "true", next: { pass: gather, fail: gather } }
    b: { type: check, run: "true", next: { pass: gather, fail: gather } }
    gather:
      type: join
      next: { done: $success }
    orphan:
      type: join
      next: { done: $success }
`)
	all := strings.Join(issues, "\n")
	for _, want := range []string{
		`node prep.next: "gather" is the join of "checks"; only its branches may route to it`,
		"node orphan: join node without a parallel node",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}
