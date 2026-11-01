package engine_test

import (
	"strings"
	"testing"
)

// A reviewer sees what the flow still checks after it, so it does not block
// on what only a later step (here a production check) can verify.
func TestContextListsLaterSteps(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: build it
      outcomes: [done]
      next: { done: review }
    review:
      type: llm
      model: ` + testModel + `
      prompt: review it
      outcomes: [approve, changes]
      max_visits: 2
      next: { approve: prod_check, changes: work }
    prod_check:
      type: check
      description: Fetch the live pages as a crawler and compare visit counters.
      run: ./check-prod.sh
      next: { pass: $success, fail: $fail }
`)
	h.finish(id, "done", nil)
	v, _ := h.st.LastVisit(h.ctx, id)
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["review"], v.Seq)
	later := md[strings.Index(md, "## Later in this flow"):]
	if !strings.Contains(later, "- prod_check (check): Fetch the live pages as a crawler") || strings.Contains(later, "- work ") {
		t.Fatalf("later steps:\n%s", md)
	}
}

// A read-only verification step on a repeat pass is told how to converge;
// the step that writes code is not.
func TestContextConvergesRepeatVerification(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      grants: [repo/ai-flow-sandbox:write]
      prompt: build it
      outcomes: [done]
      max_visits: 3
      next: { done: qa }
    qa:
      type: agent
      model: ` + testModel + `
      prompt: check it
      outcomes: [passed, defects]
      max_visits: 3
      next: { passed: $success, defects: work }
`)
	h.finish(id, "done", nil)
	md := func(node string) string {
		v, _ := h.st.LastVisit(h.ctx, id)
		r := h.run(id)
		res, err := h.e.Resolved(h.ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return h.e.ContextMarkdown(h.ctx, r, res, res.Nodes[node], v.Seq)
	}
	if strings.Contains(md("qa"), "## Converging") {
		t.Fatal("the first pass needs no convergence rules")
	}
	h.finish(id, "defects", nil)
	if strings.Contains(md("work"), "## Converging") {
		t.Fatal("the code-writing step must not get them")
	}
	h.finish(id, "done", nil)
	if got := md("qa"); !strings.Contains(got, "already judged this work 1 time(s)") {
		t.Fatalf("second QA pass:\n%s", got)
	}
}

// Agent steps are told which runtime they run on and what it has.
func TestContextNamesTheRuntime(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: qa
  nodes:
    qa:
      type: agent
      model: ` + testModel + `
      runtime: agent-web
      prompt: check it
      outcomes: [passed]
      next: { passed: $success }
`)
	v, _ := h.st.LastVisit(h.ctx, id)
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["qa"], v.Seq); !strings.Contains(md, "## Runtime\n\nThis step runs on `agent-web`: agent-go plus headless Chromium and `browse`") {
		t.Fatalf("context:\n%s", md)
	}
}

// Agent steps are told their clone is current and that they cannot fetch.
func TestContextSaysTheCloneIsCurrent(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + `  start: work
  nodes:
    work:
      type: agent
      model: ` + testModel + `
      prompt: build it
      outcomes: [done]
      next: { done: $success }
`)
	v, _ := h.st.LastVisit(h.ctx, id)
	r := h.run(id)
	res, err := h.e.Resolved(h.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if md := h.e.ContextMarkdown(h.ctx, r, res, res.Nodes["work"], v.Seq); !strings.Contains(md, "fresh clone made when this step started") || !strings.Contains(md, "Do not fetch, pull or push") {
		t.Fatalf("context:\n%s", md)
	}
}
