package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func (h *harness) restart() {
	h.t.Helper()
	if err := h.st.Close(); err != nil {
		h.t.Fatal(err)
	}
	var err error
	h.st, err = store.Open(h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.e = engine.New(h.cfg, h.st, h.l, hub.New(), nil)
	h.e.OrphanGrace = 0
}

func (h *harness) queue(src string) *store.Run {
	h.t.Helper()
	if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", YAML: header + src}); err != nil {
		h.t.Fatal(err)
	}
	r, err := h.e.CreateRun(h.ctx, "f", 0, "")
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestartPendingAndRunningNonPodVisits(t *testing.T) {
	for _, status := range []string{store.VisitPending, store.VisitRunning} {
		for _, typ := range []string{"gate", "switch", "action"} {
			t.Run(typ+"/"+status, func(t *testing.T) {
				h := newHarness(t)
				node := "      type: gate\n      prompt: approve\n      outcomes: [done]\n"
				if typ == "switch" {
					node = "      type: switch\n      cases: [{when: 'true', outcome: done}]\n      default: done\n"
				}
				if typ == "action" {
					node = "      type: action\n      action: open_pull_request\n"
				}
				r := h.queue("  start: step\n  nodes:\n    step:\n" + node + "      next: {done: $success}\n")
				v, err := h.st.AddVisit(h.ctx, r.ID, "step", typ)
				must(t, err)
				must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"status": status}))
				// A persisted URL is not a substitute for reconciling this action's
				// title/body against the existing branch PR.
				if typ == "action" {
					must(t, h.st.UpdateRun(h.ctx, r.ID, map[string]any{"pr_url": "https://example.test/o/r/pull/42"}))
				}
				h.restart()
				if typ == "action" {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						switch {
						case strings.Contains(req.URL.Path, "/branches/"):
							fmt.Fprint(w, `{}`)
						case req.Method == "GET":
							fmt.Fprint(w, `[{"number":42,"html_url":"https://example.test/o/r/pull/42"}]`)
						case req.Method == "PATCH":
							fmt.Fprint(w, `{"number":42,"html_url":"https://example.test/o/r/pull/42"}`)
						default:
							t.Errorf("unexpected request %s", req.Method)
							w.WriteHeader(500)
						}
					}))
					defer server.Close()
					h.e = engine.New(h.cfg, h.st, h.l, hub.New(), github.New(server.URL, ""))
				}
				h.e.Tick(h.ctx)
				got := h.run(r.ID)
				want := store.RunSucceeded
				if typ == "gate" {
					want = store.RunWaiting
				}
				if got.Status != want {
					t.Fatalf("status %s: %s", got.Status, got.Error)
				}
				visits, err := h.st.Visits(h.ctx, r.ID)
				must(t, err)
				if len(visits) != 1 {
					t.Fatalf("duplicated visit: %+v", visits)
				}
				if typ == "action" && !strings.Contains(string(visits[0].Outputs), `"number":42`) {
					t.Fatalf("lost PR outputs: %s", visits[0].Outputs)
				}
			})
		}
	}
}

func TestRestartAfterRunStartedBeforeFirstVisit(t *testing.T) {
	h := newHarness(t)
	r := h.queue("  start: gate\n  nodes:\n    gate:\n      type: gate\n      outcomes: [done]\n      next: {done: $success}\n")
	must(t, h.st.SetRunState(h.ctx, r, store.RunRunning, "", store.Now()))
	h.restart()
	h.e.Tick(h.ctx)
	if got := h.run(r.ID); got.Status != store.RunWaiting {
		t.Fatalf("got %s: %s", got.Status, got.Error)
	}
}

type commentHooks struct{ calls int }

func (*commentHooks) RunStarted(context.Context, *store.Task, *store.Run)  {}
func (*commentHooks) RunFinished(context.Context, *store.Task, *store.Run) {}
func (h *commentHooks) Comment(context.Context, *store.Task, string) error { h.calls++; return nil }

func TestInterruptedCommentIsNotDeliveredTwice(t *testing.T) {
	h := newHarness(t)
	r := h.queue("  start: comment\n  nodes:\n    comment:\n      type: action\n      action: comment_task\n      with: {body: hello}\n      next: {done: $success}\n")
	must(t, h.st.CreateTask(h.ctx, &store.Task{ID: "task", Source: "test", Title: "task"}))
	must(t, h.st.UpdateRun(h.ctx, r.ID, map[string]any{"task_id": "task"}))
	v, err := h.st.AddVisit(h.ctx, r.ID, "comment", "action")
	must(t, err)
	must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"status": store.VisitRunning}))
	h.restart()
	hooks := &commentHooks{}
	h.e.SetHooks(hooks)
	h.e.Tick(h.ctx)
	got := h.run(r.ID)
	if hooks.calls != 0 || got.Status != store.RunFailed || !strings.Contains(got.Error, "delivery uncertain") {
		t.Fatalf("calls=%d run=%+v", hooks.calls, got)
	}
}

const checkFlow = "  start: check\n  nodes:\n    check:\n      type: check\n      run: 'true'\n      next: {pass: $success, fail: $fail}\n"

type uncertainLauncher struct {
	*fakeLauncher
	fail bool
}

func (l *uncertainLauncher) Launch(ctx context.Context, spec engine.LaunchSpec) (string, error) {
	name, err := l.fakeLauncher.Launch(ctx, spec)
	if l.fail {
		return "", errors.New("response lost after job created")
	}
	return name, err
}

func TestRestartReconcilesLaunchResponseLost(t *testing.T) {
	h := newHarness(t)
	r := h.queue(checkFlow)
	h.e = engine.New(h.cfg, h.st, &uncertainLauncher{fakeLauncher: h.l, fail: true}, hub.New(), nil)
	h.e.Tick(h.ctx)
	v, err := h.st.LastVisit(h.ctx, r.ID)
	must(t, err)
	if v.LaunchState != "pending" || v.JobName == "" || v.Deadline == 0 {
		t.Fatalf("missing launch intent: %+v", v)
	}
	h.restart()
	h.e.Tick(h.ctx)
	v, err = h.st.LastVisit(h.ctx, r.ID)
	must(t, err)
	if v.LaunchState != "launched" || len(h.l.launched) != 1 {
		t.Fatalf("duplicate launch: state=%s calls=%d", v.LaunchState, len(h.l.launched))
	}
	h.finish(r.ID, "pass", nil)
	if h.run(r.ID).Status != store.RunSucceeded {
		t.Fatal("did not finish")
	}
}

func TestRestartLaunchIntentBeforeSubmission(t *testing.T) {
	h := newHarness(t)
	r := h.queue(checkFlow)
	v, err := h.st.AddVisit(h.ctx, r.ID, "check", "check")
	must(t, err)
	name := engine.JobName(engine.LaunchSpec{RunID: r.ID, Seq: v.Seq, Node: v.Node})
	must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"launch_state": "pending", "job_name": name, "deadline": time.Now().Add(time.Minute).UnixMilli()}))
	h.restart()
	h.e.Tick(h.ctx)
	if len(h.l.launched) != 1 {
		t.Fatalf("launches %d", len(h.l.launched))
	}
	v, err = h.st.LastVisit(h.ctx, r.ID)
	must(t, err)
	if v.LaunchState != "launched" {
		t.Fatalf("launch state %q", v.LaunchState)
	}
}

func TestSnapshotPinsDefaultsAndImageAcrossRestart(t *testing.T) {
	h := newHarness(t)
	r := h.queue(checkFlow)
	before, err := h.e.Resolved(h.ctx, r)
	must(t, err)
	runtime := before.Nodes["check"].Runtime
	image := h.cfg.Catalog.Runtimes[runtime].Image
	h.cfg.Catalog.Defaults.Timeout.Duration = 19 * time.Second
	rt := h.cfg.Catalog.Runtimes[runtime]
	rt.Image = "changed:latest"
	h.cfg.Catalog.Runtimes[runtime] = rt
	h.restart()
	h.e.Tick(h.ctx)
	if len(h.l.launched) != 1 {
		t.Fatalf("launches %d", len(h.l.launched))
	}
	spec := h.l.launched[0]
	if spec.Image != image || spec.Timeout != before.Nodes["check"].Timeout.Duration {
		t.Fatalf("drift changed launch: %+v", spec)
	}
}

func TestSnapshotRejectsCatalogDriftAndCurrentAuthorizationRevocation(t *testing.T) {
	h := newHarness(t)
	r := h.queue(askFlow)
	m := h.cfg.Catalog.Models[testModel]
	original := m.Model
	m.Model = "different-model"
	if _, err := h.e.Resolved(h.ctx, r); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected explicit drift error, got %v", err)
	}
	m.Model = original
	h.cfg.Projects["sandbox"].Spec.Allow.Models = []string{"some-other-model"}
	if _, err := h.e.Resolved(h.ctx, r); err == nil || !strings.Contains(err.Error(), "authorized") {
		t.Fatalf("revocation ignored: %v", err)
	}
}

func TestSnapshotDoesNotPersistEnvironmentSecrets(t *testing.T) {
	h := newHarness(t)
	t.Setenv("SNAPSHOT_TEST_SECRET", "do-not-persist-me")
	h.cfg.Env.MCP.Servers = map[string]config.MCPServer{"private": {URL: "https://example.test", Headers: map[string]string{"Authorization": "do-not-persist-me"}}}
	h.cfg.Env.GitHub.TokenEnv = "SNAPSHOT_TEST_SECRET"
	r := h.queue(checkFlow)
	if strings.Contains(string(r.Snapshot), "do-not-persist-me") {
		t.Fatal("secret persisted")
	}
	var data map[string]any
	must(t, json.Unmarshal(r.Snapshot, &data))
	if data["version"] != float64(1) {
		t.Fatalf("snapshot version: %v", data["version"])
	}
}

func TestPRCreationResponseLostReconcilesByBranch(t *testing.T) {
	h := newHarness(t)
	r := h.queue("  start: pr\n  nodes:\n    pr:\n      type: action\n      action: open_pull_request\n      next: {done: $success}\n")
	created, posts := false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/branches/"):
			fmt.Fprint(w, `{}`)
		case req.Method == "GET":
			if created {
				fmt.Fprint(w, `[{"number":42,"html_url":"https://example.test/o/r/pull/42"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case req.Method == "POST":
			posts++
			created = true
			// The remote side committed; the caller sees a failed response.
			w.WriteHeader(500)
		case req.Method == "PATCH":
			fmt.Fprint(w, `{"number":42,"html_url":"https://example.test/o/r/pull/42"}`)
		default:
			t.Errorf("unexpected request %s %s", req.Method, req.URL)
		}
	}))
	defer server.Close()
	gh := github.New(server.URL, "")
	h.e = engine.New(h.cfg, h.st, h.l, hub.New(), gh)
	h.e.Tick(h.ctx)
	if got := h.run(r.ID); got.Status != store.RunRunning {
		t.Fatalf("not retryable: %+v", got)
	}
	v, err := h.st.LastVisit(h.ctx, r.ID)
	must(t, err)
	started := store.Now() - 1000
	must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"started_at": started}))
	h.restart()
	h.e = engine.New(h.cfg, h.st, h.l, hub.New(), gh)
	h.e.Tick(h.ctx)
	got := h.run(r.ID)
	if got.Status != store.RunSucceeded || posts != 1 || got.PRURL == "" {
		t.Fatalf("posts=%d run=%+v", posts, got)
	}
	v, err = h.st.LastVisit(h.ctx, r.ID)
	must(t, err)
	if v.StartedAt != started {
		t.Fatalf("PR reconciliation reset duration start: %d != %d", v.StartedAt, started)
	}
}

func TestLegacySplitGateStateIsRepaired(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + "  start: gate\n  nodes:\n    gate:\n      type: gate\n      outcomes: [done]\n      next: {done: $success}\n")
	must(t, h.st.UpdateRun(h.ctx, id, map[string]any{"status": store.RunRunning}))
	h.restart()
	h.e.Tick(h.ctx)
	if h.run(id).Status != store.RunWaiting {
		t.Fatal("gate/run split not reconciled")
	}
}

func TestLegacySplitCancellationIsRepaired(t *testing.T) {
	h := newHarness(t)
	id := h.flow(header + checkFlow)
	v, err := h.st.LastVisit(h.ctx, id)
	must(t, err)
	must(t, h.st.UpdateVisit(h.ctx, id, v.Seq, map[string]any{"status": store.VisitCanceled}))
	h.restart()
	h.e.Tick(h.ctx)
	if h.run(id).Status != store.RunCanceled {
		t.Fatal("canceled visit stranded its run")
	}
}

func TestInterruptedLocalLaunchDoesNotDuplicateUnknownProcess(t *testing.T) {
	h := newHarness(t)
	h.cfg.Env.Runs.Local = true
	r := h.queue(checkFlow)
	v, err := h.st.AddVisit(h.ctx, r.ID, "check", "check")
	must(t, err)
	name := engine.JobName(engine.LaunchSpec{RunID: r.ID, Seq: v.Seq, Node: v.Node})
	must(t, h.st.UpdateVisit(h.ctx, r.ID, v.Seq, map[string]any{"job_name": name, "launch_state": "pending"}))
	h.restart()
	h.e.Tick(h.ctx)
	got := h.run(r.ID)
	if len(h.l.launched) != 0 || got.Status != store.RunFailed || !strings.Contains(got.Error, "local launch delivery uncertain") {
		t.Fatalf("launches=%d run=%+v", len(h.l.launched), got)
	}
}

func TestLegacySnapshotAdoptionIsDurable(t *testing.T) {
	h := newHarness(t)
	r := h.queue(checkFlow)
	must(t, h.st.UpdateRun(h.ctx, r.ID, map[string]any{"snapshot": ""}))
	r = h.run(r.ID)
	before, err := h.e.Resolved(h.ctx, r)
	must(t, err)
	h.cfg.Catalog.Defaults.Timeout.Duration = 7 * time.Second
	h.restart()
	after, err := h.e.Resolved(h.ctx, h.run(r.ID))
	must(t, err)
	if before.Nodes["check"].Timeout != after.Nodes["check"].Timeout {
		t.Fatal("legacy snapshot was resolved again")
	}
}
