package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/store"
)

const shipNodes = `    ship:
      type: action
      action: merge_pull_request
      next: { merged: live, conflict: $fail, blocked: $fail, timeout: $fail }
    live:
      type: action
      action: wait_for_deploy
      next: { deployed: soak, timeout: $fail }
    soak:
      type: action
      action: check_health
      with: { duration: 1h }
      next: { healthy: $success, degraded: $fail, timeout: $fail }
`

func greenWorld() *fakeWorld {
	return &fakeWorld{state: "clean", mergeable: true, version: "oldsha0000000", metric: "0", appStatus: 200,
		checks: []map[string]any{{"name": "t", "status": "completed", "conclusion": "success"}}}
}

func TestProtectedPathsAreNotMerged(t *testing.T) {
	for file, blocked := range map[string]bool{".github/workflows/ci.yml": true, "main.go": false} {
		w := greenWorld()
		w.files = []map[string]any{{"filename": "a.go"}, {"filename": file}}
		h, id := releaseFlow(t, w, func(d *config.Deploy) { d.Protected = []string{".github/**"} }, shipNodes)
		h.ticks(3)
		if blocked != strings.HasSuffix(h.path(id), "ship:blocked") || (w.merges == 0) != blocked {
			t.Errorf("%s: path %s, merges %d", file, h.path(id), w.merges)
		}
	}
	// Moving a protected file away counts too.
	w := greenWorld()
	w.files = []map[string]any{{"filename": "x.yml", "previous_filename": ".github/workflows/ci.yml"}}
	h, id := releaseFlow(t, w, func(d *config.Deploy) { d.Protected = []string{".github/**"} }, shipNodes)
	h.ticks(3)
	if w.merges != 0 {
		t.Errorf("renamed protected file merged: %s", h.path(id))
	}
}

func TestUnreviewedChangeIsNotMerged(t *testing.T) {
	w := greenWorld()
	h, id := releaseFlow(t, w, func(d *config.Deploy) {}, shipNodes)
	// The run's own snapshot pins the project, as it does for a real run.
	h.cfg.Projects["sandbox"].Spec.Review = &config.Review{Required: true}
	must(t, h.st.UpdateRun(h.ctx, id, map[string]any{"snapshot": ""}))
	h.ticks(3)
	if w.merges != 0 {
		t.Fatalf("merged without a review: %s", h.path(id))
	}
}

func TestOnlyPipelineJobsFailADeploy(t *testing.T) {
	for pipeline, ends := range map[string]bool{"release": false, "ios": true, "": true} {
		w := greenWorld()
		w.mainCI = []map[string]any{{"id": 77, "name": "ios", "status": "completed", "conclusion": "failure"}}
		h, id := releaseFlow(t, w, func(d *config.Deploy) {
			if pipeline != "" {
				d.Pipeline = []string{pipeline}
			}
		}, shipNodes)
		h.ticks(4)
		if ended := strings.HasSuffix(h.path(id), "live:timeout"); ended != ends {
			t.Errorf("pipeline %q: path %s", pipeline, h.path(id))
		}
	}
}

func TestProjectNotifySettings(t *testing.T) {
	h := newHarness(t)
	f := h.notifier(config.NotifyGate, config.NotifyFailed)
	h.cfg.Projects["sandbox"].Spec.Notify = &config.ProjectNotify{Events: []string{config.NotifySucceeded}}
	h.flow(gateFlow) // opens a gate: not one of the project's events
	if got := f.titles(t, 1); len(got) != 0 {
		t.Fatalf("gate event sent: %v", got)
	}
	h2 := newHarness(t)
	f2 := h2.notifier(config.NotifyGate, config.NotifyFailed)
	h2.cfg.Projects["sandbox"].Spec.Notify = &config.ProjectNotify{Summaries: true}
	id := h2.flow(gateFlow)
	v, _ := h2.st.LastVisit(h2.ctx, id)
	must(t, h2.e.Decide(h2.ctx, id, v.Seq, "reject", "t", ""))
	if h2.run(id).Status != store.RunFailed {
		t.Fatal("not failed")
	}
	// The gate is announced; how the run ended is the app's to summarize.
	if got := f2.titles(t, 2); len(got) != 1 {
		t.Fatalf("sent %v", got)
	}
}

func TestPageProbesWatchTheContent(t *testing.T) {
	for page, want := range map[string]string{"<title>Bills": "soak:healthy", "<div id=root>": "soak:degraded"} {
		w := greenWorld()
		w.version, w.page = "mergedsha0000001", page
		h, id := releaseFlow(t, w, func(d *config.Deploy) {
			base := strings.TrimSuffix(d.URL, "/app")
			d.Probes = []config.PageProbe{{URL: base + "/page", Contains: "<title>Bills"}}
		}, strings.ReplaceAll(shipNodes, "duration: 1h", "duration: 20ms"))
		for range 8 {
			h.ticks(1)
			time.Sleep(10 * time.Millisecond)
		}
		if got := h.path(id); !strings.HasSuffix(got, want) {
			t.Errorf("page %q: %s, want %s", page, got, want)
		}
	}
}
