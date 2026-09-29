package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
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
