package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/notify"
	"github.com/mauza/ai-flow/internal/store"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

// titles waits briefly for background sends, then returns what arrived.
func (f *fakeSender) titles(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.sent)
		var out []string
		for _, m := range f.sent {
			out = append(out, m.Title)
		}
		f.mu.Unlock()
		if n >= want || time.Now().After(deadline) {
			time.Sleep(20 * time.Millisecond) // catch unexpected extras
			f.mu.Lock()
			defer f.mu.Unlock()
			out = out[:0]
			for _, m := range f.sent {
				out = append(out, m.Title)
			}
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) notifier(events ...string) *fakeSender {
	f := &fakeSender{}
	h.cfg.Env.Server.PublicURL = "http://ai-flow:8080/"
	h.cfg.Env.Notify = config.Notify{Ntfy: &config.Ntfy{URL: "http://ntfy", Topic: "t"}, Events: events, StuckAfter: flow.Duration{Duration: time.Hour}}
	h.e.SetNotifier(f)
	return f
}

const gateFlow = header + `  start: approve
  nodes:
    approve:
      type: gate
      prompt: Ship it?
      outcomes: [approve, reject]
      next: { approve: $success, reject: $fail }
`

func TestNotifiesOnGateAndFailure(t *testing.T) {
	h := newHarness(t)
	f := h.notifier(config.NotifyGate, config.NotifyStuck, config.NotifyFailed)
	id := h.flow(gateFlow)
	v, _ := h.st.LastVisit(h.ctx, id)
	if err := h.e.Decide(h.ctx, id, v.Seq, "reject", "t", ""); err != nil {
		t.Fatal(err)
	}
	got := f.titles(t, 2)
	if strings.Join(got, " | ") != "f: approve needs a decision | f: failed" {
		t.Fatalf("sent %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.sent[0]; m.Body != "Ship it?" || m.Click != "http://ai-flow:8080/runs/"+id || m.Priority != 4 {
		t.Errorf("gate message %+v", m)
	}
	if m := f.sent[1]; !strings.Contains(m.Body, "approve → reject") {
		t.Errorf("failure message %+v", m)
	}
}

func TestStuckGateNotifiesOnce(t *testing.T) {
	h := newHarness(t)
	f := h.notifier(config.NotifyStuck)
	id := h.flow(gateFlow)
	h.e.Tick(h.ctx)
	if got := f.titles(t, 0); len(got) != 0 {
		t.Fatalf("a fresh gate is not stuck: %q", got)
	}
	v, _ := h.st.LastVisit(h.ctx, id)
	if err := h.st.UpdateVisit(h.ctx, id, v.Seq, map[string]any{"started_at": store.Now() - 2*time.Hour.Milliseconds()}); err != nil {
		t.Fatal(err)
	}
	h.e.Tick(h.ctx)
	h.e.Tick(h.ctx)
	if got := f.titles(t, 1); strings.Join(got, "|") != "f: approve still waiting after 1h" {
		t.Fatalf("sent %q", got)
	}
	if r := h.run(id); r.Status != store.RunWaiting {
		t.Fatalf("a reminder must not change the run: %s", r.Status)
	}
}

func TestNoNotificationsForUnselectedEvents(t *testing.T) {
	h := newHarness(t)
	f := h.notifier(config.NotifySucceeded)
	id := h.flow(gateFlow)
	v, _ := h.st.LastVisit(h.ctx, id)
	if err := h.e.Decide(h.ctx, id, v.Seq, "approve", "t", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.titles(t, 1); strings.Join(got, "|") != "f: succeeded" {
		t.Fatalf("sent %q", got)
	}
}

func TestShortDurationReadsNaturally(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 300 * time.Millisecond: "300ms", 45 * time.Second: "45s",
		10 * time.Minute: "10m", 90 * time.Minute: "1h30m", 4 * time.Hour: "4h"} {
		if got := engine.ShortDuration(d); got != want {
			t.Errorf("%s: got %q want %q", d, got, want)
		}
	}
}

type linkHooks struct{ posted []string }

func (c *linkHooks) RunStarted(context.Context, *store.Task, *store.Run)  {}
func (c *linkHooks) RunFinished(context.Context, *store.Task, *store.Run) {}
func (c *linkHooks) Comment(_ context.Context, t *store.Task, body string) error {
	c.posted = append(c.posted, t.ID)
	return nil
}

func TestCommentTaskOnlyClaimsALinkedTask(t *testing.T) {
	for _, linked := range []bool{false, true} {
		h := newHarness(t)
		hooks := &linkHooks{}
		h.e.SetHooks(hooks)
		task := &store.Task{ID: "t-1", Source: "manual", Title: "x"}
		if linked {
			task.Source, task.ExternalID, task.Identifier = "linear", "uuid", "MAU-9"
		}
		if err := h.st.CreateTask(h.ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := h.st.SaveFlow(h.ctx, &store.FlowVersion{Name: "f", Project: "sandbox", YAML: header + `  start: say
  nodes:
    say: { type: action, action: comment_task, with: { body: hi }, next: { done: $success } }
`}); err != nil {
			t.Fatal(err)
		}
		r, err := h.e.CreateRun(h.ctx, "f", 0, "t-1")
		if err != nil {
			t.Fatal(err)
		}
		h.e.Tick(h.ctx)
		v := h.current(r.ID)
		want := "No linked task to comment on"
		if linked {
			want = "Commented on MAU-9"
		}
		if v.Summary != want || len(hooks.posted) != map[bool]int{false: 0, true: 1}[linked] {
			t.Errorf("linked=%v: summary %q, posted %v", linked, v.Summary, hooks.posted)
		}
	}
}
