package engine_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func (h *harness) execOfflineSQL(query string) {
	h.t.Helper()
	must(h.t, h.st.Close())
	db, err := sql.Open("sqlite", h.dbPath)
	must(h.t, err)
	_, err = db.Exec(query)
	closeErr := db.Close()
	must(h.t, err)
	must(h.t, closeErr)
	h.restart()
}

func TestTransientStartFailureDoesNotConsumeAdmissionSlot(t *testing.T) {
	h := newHarness(t)
	h.cfg.Env.Runs.MaxConcurrent = 1
	old := h.queue(checkFlow)
	good := h.queue(checkFlow)
	h.execOfflineSQL(fmt.Sprintf(`CREATE TRIGGER temporarily_block_start BEFORE UPDATE ON runs WHEN NEW.id = '%s' AND NEW.status = 'running' BEGIN SELECT RAISE(ABORT, 'temporary database failure'); END`, old.ID))
	h.e.Tick(h.ctx)
	if h.run(old.ID).Status != store.RunQueued || h.run(good.ID).Status != store.RunRunning {
		t.Fatal("transient pre-admission failure consumed the available slot")
	}
	h.execOfflineSQL(`DROP TRIGGER temporarily_block_start`)
	h.finish(good.ID, "pass", nil)
	if h.run(old.ID).Status != store.RunRunning {
		t.Fatal("transient start did not recover when capacity freed")
	}
}

func TestTransientLaunchFailureStillOccupiesAdmissionSlot(t *testing.T) {
	h := newHarness(t)
	h.cfg.Env.Runs.MaxConcurrent = 1
	old := h.queue(checkFlow)
	good := h.queue(checkFlow)
	h.e = engine.New(h.cfg, h.st, &uncertainLauncher{fakeLauncher: h.l, fail: true}, hub.New(), nil)
	h.e.Tick(h.ctx)
	if h.run(old.ID).Status != store.RunRunning || h.run(good.ID).Status != store.RunQueued || len(h.l.launched) != 1 {
		t.Fatal("uncertain running launch released its admission slot")
	}
}

const prFlow = "  start: pr\n  nodes:\n    pr:\n      type: action\n      action: open_pull_request\n      next: {done: $success}\n"
const askFlow = "  start: ask\n  nodes:\n    ask:\n      type: llm\n      model: " + testModel + "\n      prompt: hello\n      outcomes: [done]\n      next: {done: $success}\n"

func TestRejectedOldestRunDoesNotStarveQueue(t *testing.T) {
	for _, reason := range []string{"drift", "revoked", "revoked-grant", "missing-github", "invalid-snapshot"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Env.Runs.MaxConcurrent = 1
			src := askFlow
			if reason == "missing-github" {
				src = prFlow
			}
			bad := h.queue(src)
			switch reason {
			case "drift":
				h.cfg.Catalog.Models[testModel].Model = "changed"
			case "revoked":
				h.cfg.Projects["sandbox"].Spec.Allow.Models = []string{"different-model"}
			case "revoked-grant":
				h.cfg.Projects["sandbox"].Spec.Allow.Grants = []string{"different-grant"}
				h.cfg.Projects["sandbox"].Spec.Repo = "" // newer checks can run without the revoked repo
			case "invalid-snapshot":
				must(t, h.st.UpdateRun(h.ctx, bad.ID, map[string]any{"snapshot": `{"version":999}`}))
			}
			good := h.queue(checkFlow)
			h.e.Tick(h.ctx)
			if got := h.run(bad.ID); got.Status != store.RunFailed {
				t.Fatalf("permanent rejection still %s: %s", got.Status, got.Error)
			}
			if got := h.run(good.ID); got.Status != store.RunRunning {
				t.Fatalf("valid newer run starved: %s", got.Status)
			}
			if len(h.l.launched) != 1 || h.l.launched[0].RunID != good.ID {
				t.Fatalf("launches %+v", h.l.launched)
			}
		})
	}
}

func TestDeletedTaskDoesNotPreventRunLifecycleOrTemplates(t *testing.T) {
	for _, action := range []string{"finish", "cancel"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			must(t, h.st.CreateTask(h.ctx, &store.Task{ID: "task", Source: "test", Title: "old title"}))
			r := h.queue("  start: gate\n  nodes:\n    gate:\n      type: gate\n      prompt: '${{ task.title }}'\n      outcomes: [done]\n      next: {done: comment}\n    comment:\n      type: action\n      action: comment_task\n      next: {done: $success}\n")
			must(t, h.st.UpdateRun(h.ctx, r.ID, map[string]any{"task_id": "task"}))
			must(t, h.st.DeleteTask(h.ctx, "task"))
			h.e.Tick(h.ctx)
			if got := h.run(r.ID); got.Status != store.RunWaiting {
				t.Fatalf("deleted task blocked start/gate: %+v", got)
			}
			v, err := h.st.LastVisit(h.ctx, r.ID)
			must(t, err)
			if v.Prompt != "f" {
				t.Fatalf("missing task fallback: %q", v.Prompt)
			}
			want := store.RunSucceeded
			if action == "cancel" {
				must(t, h.e.Cancel(h.ctx, r.ID))
				want = store.RunCanceled
			} else {
				must(t, h.e.Decide(h.ctx, r.ID, v.Seq, "done", "test", ""))
			}
			if got := h.run(r.ID); got.Status != want {
				t.Fatalf("deleted task blocked completion: %+v", got)
			}
		})
	}
}

func TestPRPermanentFailuresEndRun(t *testing.T) {
	for _, code := range []int{404, 401, 422} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHarness(t)
			r := h.queue(prFlow)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { calls++; w.WriteHeader(code) }))
			defer server.Close()
			h.e = engine.New(h.cfg, h.st, h.l, hub.New(), github.New(server.URL, ""))
			h.e.Tick(h.ctx)
			h.e.Tick(h.ctx)
			if got := h.run(r.ID); got.Status != store.RunFailed || calls != 1 {
				t.Fatalf("permanent PR error retried: calls=%d run=%+v", calls, got)
			}
			v, err := h.st.LastVisit(h.ctx, r.ID)
			must(t, err)
			if v.Status != store.VisitError {
				t.Fatalf("failed run left active visit: %+v", v)
			}
		})
	}
}

func TestRepeatedPRActionsUpdateExistingPRMetadata(t *testing.T) {
	h := newHarness(t)
	r := h.queue("  start: first\n  nodes:\n    first:\n      type: action\n      action: open_pull_request\n      with: {title: initial, body: initial-body}\n      next: {done: second}\n    second:\n      type: action\n      action: open_pull_request\n      with: {title: revised, body: revised-body}\n      next: {done: $success}\n")
	posts, patches := 0, 0
	var title, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.Contains(req.URL.Path, "/branches/"):
			fmt.Fprint(w, `{}`)
		case req.Method == "GET":
			if posts > 0 {
				fmt.Fprint(w, `[{"number":42,"html_url":"https://example.test/o/r/pull/42"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case req.Method == "POST", req.Method == "PATCH":
			if req.Method == "POST" {
				posts++
			} else {
				patches++
			}
			var payload struct{ Title, Body string }
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			title, body = payload.Title, payload.Body
			fmt.Fprint(w, `{"number":42,"html_url":"https://example.test/o/r/pull/42"}`)
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
	}))
	defer server.Close()
	h.e = engine.New(h.cfg, h.st, h.l, hub.New(), github.New(server.URL, ""))
	h.e.Tick(h.ctx)
	if got := h.run(r.ID); got.Status != store.RunSucceeded {
		t.Fatalf("run %+v", got)
	}
	if posts != 1 || patches != 1 || title != "revised" || !strings.HasPrefix(body, "revised-body") {
		t.Fatalf("posts=%d patches=%d title=%q body=%q", posts, patches, title, body)
	}
}
