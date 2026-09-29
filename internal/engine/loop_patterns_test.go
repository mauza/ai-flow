package engine_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/tmpl"
)

func readLoopExample(t *testing.T, path string) (string, *flow.Flow) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := flow.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return string(src), f
}

func TestLoopExamplesValidate(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("../../examples/loops/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("loop examples: %v, %v", paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			_, f := readLoopExample(t, path)
			r := resolve.Resolve(f, cfg)
			if issues := resolve.Validate(r, cfg); len(issues) > 0 {
				t.Fatalf("example must have no errors or warnings: %v", issues)
			}
			for id, n := range r.Nodes {
				if n.MaxVisits <= 0 || n.OnExhausted == "" {
					t.Errorf("%s lacks explicit repeat bounds", id)
				}
				for name, typ := range n.Outputs {
					if _, err := resolve.OutputSchema(typ); err != nil {
						t.Errorf("%s.outputs.%s: %v", id, name, err)
					}
				}
				if flow.PodType(n.Type) && n.Timeout.Duration != 20*time.Minute {
					t.Errorf("%s lost the catalog pod timeout", id)
				}
				if n.Type != flow.TypeGate {
					continue
				}
				if n.Timeout.Duration != 24*time.Hour || n.Next["timeout"] != flow.Fail || n.Runtime != "" {
					t.Errorf("%s: gate must have its own 24h timeout, failure route, and no pod runtime", id)
				}
				// Removing just the explicit gate timeout must not inherit the
				// catalog's 20m pod default (or the human-decision preset's bounds).
				saved := f.Spec.Nodes[id].Timeout
				f.Spec.Nodes[id].Timeout = flow.Duration{}
				bare := resolve.Resolve(f, cfg).Nodes[id]
				if bare.Timeout.Duration != 0 || slices.Contains(bare.Outcomes, flow.OutcomeTimeout) {
					t.Errorf("%s inherited a pod timeout", id)
				}
				f.Spec.Nodes[id].Timeout = saved
			}
		})
	}
}

func TestLoopConditionRequiresBooleanAndCheckEvidence(t *testing.T) {
	_, f := readLoopExample(t, "../../examples/loops/repeat-until-goal.yaml")
	condition := f.Spec.Nodes["condition"]
	if f.Spec.Nodes["assess"].Outputs["goal_met"] != "bool" {
		t.Fatal("completion signal must be a structured boolean")
	}
	for _, tc := range []struct {
		check string
		goal  bool
		want  string
	}{
		{"pass", false, "repeat"}, {"pass", true, "complete"},
		{"fail", true, "repeat"}, {"fail", false, "repeat"},
	} {
		ctx := map[string]any{"nodes": map[string]any{
			"verify": map[string]any{"outcome": tc.check},
			"assess": map[string]any{"outputs": map[string]any{"goal_met": tc.goal}},
		}}
		got := condition.Default
		for _, c := range condition.Cases {
			if err := resolve.CompileCEL(c.When); err != nil {
				t.Fatal(err)
			}
			match, err := resolve.EvalCEL(c.When, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if match {
				got = c.Outcome
				break
			}
		}
		if got != tc.want {
			t.Errorf("check=%s goal=%v: got %s, want %s", tc.check, tc.goal, got, tc.want)
		}
	}
}

func startLoopExample(h *harness, name string) string {
	h.t.Helper()
	src, f := readLoopExample(h.t, "../../examples/loops/"+name+".yaml")
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: f.Metadata.Name, Project: f.Metadata.Project, YAML: src}); err != nil {
		h.t.Fatal(err)
	}
	r, err := h.e.CreateRun(h.ctx, f.Metadata.Name, 0, "")
	if err != nil {
		h.t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	return r.ID
}

func assertLoopNode(h *harness, id, node, status string) {
	h.t.Helper()
	r := h.run(id)
	if r.CurrentNode != node || r.Status != status {
		h.t.Fatalf("want %s at %s, got %s at %s: %s; path %s", status, node, r.Status, r.CurrentNode, r.Error, h.path(id))
	}
}

func finishLoopPass(h *harness, id, check string, goal bool) {
	h.t.Helper()
	assertLoopNode(h, id, "work", store.RunRunning)
	h.finish(id, "done", map[string]any{"summary_of_change": "Focused correction"})
	assertLoopNode(h, id, "verify", store.RunRunning)
	code := 0
	if check != "pass" {
		code = 1
	}
	h.finish(id, check, map[string]any{"exit_code": code, "log_tail": "check evidence"})
	assertLoopNode(h, id, "assess", store.RunRunning)
	h.finish(id, "assessed", map[string]any{"goal_met": goal, "remaining": []string{"Recheck empty input"}})
}

func TestLoopRepeatsUntilLatestGoalIsTrue(t *testing.T) {
	for _, first := range []struct {
		name, check string
		goal        bool
	}{
		{"goal not met", "pass", false},
		{"check vetoes model", "fail", true},
	} {
		t.Run(first.name, func(t *testing.T) {
			h := newHarness(t)
			id := startLoopExample(h, "repeat-until-goal")
			finishLoopPass(h, id, first.check, first.goal)
			assertLoopNode(h, id, "work", store.RunRunning)
			res, err := h.e.Resolved(h.ctx, h.run(id))
			if err != nil {
				t.Fatal(err)
			}
			ctx := h.e.TemplateContext(h.ctx, h.run(id), res, res.Nodes["work"])
			if feedback, ok := tmpl.Lookup(ctx, []string{"inputs", "remaining"}); !ok || !strings.Contains(feedback.(string), "Recheck empty input") {
				t.Fatal("switch back edge lost named feedback")
			}
			finishLoopPass(h, id, "pass", true)
			if r := h.run(id); r.Status != store.RunSucceeded {
				t.Fatalf("latest boolean should finish the run: %+v; %s", r, h.path(id))
			}
			ctx = h.e.TemplateContext(h.ctx, h.run(id), res, res.Nodes["condition"])
			if goal, ok := tmpl.Lookup(ctx, []string{"nodes", "assess", "outputs", "goal_met"}); !ok || goal != true {
				t.Fatal("context did not use latest successful assessment")
			}
			want := "work:done verify:" + first.check + " assess:assessed condition:repeat work:done verify:pass assess:assessed condition:complete"
			if got := h.path(id); got != want {
				t.Fatalf("unexpected loop path: %s", got)
			}
			if len(h.l.launched) != 6 || h.l.launched[3].Visit != 2 {
				t.Fatalf("expected two full pod groups: %+v", h.l.launched)
			}
		})
	}
}

// Drive real persisted gate deadlines without sleeping or calling live services.
func decideLoopGate(h *harness, id, outcome string) {
	h.t.Helper()
	v, err := h.st.LastVisit(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	if v.Status != store.VisitWaiting || v.Deadline-v.StartedAt < (24*time.Hour-time.Second).Milliseconds() || v.Deadline-v.StartedAt > (24*time.Hour+time.Second).Milliseconds() {
		h.t.Fatalf("gate lacks its explicit 24h deadline: %+v", v)
	}
	if outcome == "timeout" {
		if err := h.st.UpdateVisit(h.ctx, id, v.Seq, map[string]any{"deadline": store.Now() - 1}); err != nil {
			h.t.Fatal(err)
		}
		h.e.Tick(h.ctx)
		return
	}
	if err := h.e.Decide(h.ctx, id, v.Seq, outcome, "loop-test", ""); err != nil {
		h.t.Fatal(err)
	}
}

func TestLoopExhaustionHandsOffWithoutLaunchingAgain(t *testing.T) {
	for _, decision := range []string{"stop", "timeout"} {
		t.Run(decision, func(t *testing.T) {
			h := newHarness(t)
			id := startLoopExample(h, "repeat-until-goal")
			for range 3 {
				finishLoopPass(h, id, "pass", false)
			}
			assertLoopNode(h, id, "exhausted", store.RunWaiting)
			if len(h.l.launched) != 9 {
				t.Fatalf("expected exactly three full groups: %+v", h.l.launched)
			}
			decideLoopGate(h, id, decision)
			if h.run(id).Status != store.RunFailed || !strings.HasSuffix(h.path(id), "exhausted:"+decision) {
				t.Fatalf("exhaustion did not fail via %s: %s", decision, h.path(id))
			}
		})
	}
}

func TestLoopHumanRevisionApprovalBoundsAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, last, want string
		passes           int
	}{
		{"revise then approve", "approve", store.RunSucceeded, 2},
		{"revision budget exhausted", "revise", store.RunFailed, 3},
		{"human wait expires", "timeout", store.RunFailed, 1},
		{"human stops", "stop", store.RunFailed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			id := startLoopExample(h, "human-revise-approve")
			for i := 0; i < tc.passes; i++ {
				assertLoopNode(h, id, "work", store.RunRunning)
				h.finish(id, "done", map[string]any{"summary_of_change": "Focused correction"})
				assertLoopNode(h, id, "verify", store.RunRunning)
				h.finish(id, "pass", map[string]any{"exit_code": 0, "log_tail": "tests passed"})
				assertLoopNode(h, id, "review_gate", store.RunWaiting)
				decision := "revise"
				if i == tc.passes-1 {
					decision = tc.last
				}
				decideLoopGate(h, id, decision)
			}
			if r := h.run(id); r.Status != tc.want {
				t.Fatalf("got %s: %s; path %s", r.Status, r.Error, h.path(id))
			}
			if len(h.l.launched) != 2*tc.passes {
				t.Fatal("gate decision launched unexpected work")
			}
			if tc.last == "revise" && !strings.Contains(h.run(id).Error, "work already ran 3 times") {
				t.Fatalf("human revisions did not exhaust the node-level total: %s", h.run(id).Error)
			}
		})
	}
}
