package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/notify"
	"github.com/mauza/ai-flow/internal/store"
)

type inbox struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (f *inbox) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

const releaseYAML = `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: %s, project: sandbox }
spec:
  start: ci
  nodes:
    ci: { type: action, action: wait_for_checks, next: { passed: ship, failed: $fail, none: $fail, timeout: $fail } }
    ship: { type: action, action: merge_pull_request, next: { merged: live, conflict: $fail, blocked: $fail, timeout: $fail } }
    live: { type: action, action: wait_for_deploy, next: { deployed: soak, timeout: back } }
    soak: { type: action, action: check_health, next: { healthy: $success, degraded: back, timeout: back } }
    back: { type: action, action: rollback_deploy, next: { rolled_back: $fail, failed: $fail, timeout: $fail } }
`

// releaseRun stores a finished run that took the given "node:outcome" steps.
func releaseRun(t *testing.T, a *App, name, status string, steps ...string) *store.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := a.Store.SaveFlow(ctx, &store.FlowVersion{Name: name, Project: "sandbox", YAML: fmt.Sprintf(releaseYAML, name)}); err != nil {
		t.Fatal(err)
	}
	run, err := a.Engine.CreateRun(ctx, name, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		node, outcome, _ := strings.Cut(s, ":")
		v, err := a.Store.AddVisit(ctx, run.ID, node, "action")
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Store.UpdateVisit(ctx, run.ID, v.Seq, map[string]any{"status": store.VisitSucceeded, "outcome": outcome}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Store.SetRunState(ctx, run, status, "", store.Now()); err != nil {
		t.Fatal(err)
	}
	run, _ = a.Store.GetRun(ctx, run.ID)
	return run
}

func reportApp(t *testing.T, reply string) (*App, *inbox) {
	t.Helper()
	a := setupImprovementApp(t, func(string) string { return reply })
	in := &inbox{}
	a.Notifier = in
	p := a.Cfg.Current().Projects["sandbox"]
	p.Spec.Deploy = &config.Deploy{Branch: "main", Versions: []config.VersionProbe{{URL: "http://x"}}, Rollback: &config.RollbackSpec{Workflow: "rollback.yml"}}
	p.Spec.Notify = &config.ProjectNotify{Summaries: true, Digest: &config.DigestTime{At: "08:00", TZ: "America/Denver"}}
	return a, in
}

func TestRunResults(t *testing.T) {
	a, _ := reportApp(t, "{}")
	for i, tc := range []struct {
		status, want string
		steps        []string
	}{
		{store.RunSucceeded, ResultShipped, []string{"ci:passed", "ship:merged", "live:deployed", "soak:healthy"}},
		{store.RunFailed, ResultShipped, []string{"ship:merged", "live:deployed", "soak:healthy"}}, // a later step failed
		{store.RunFailed, ResultRolledBack, []string{"ship:merged", "live:deployed", "soak:degraded", "back:rolled_back"}},
		{store.RunFailed, ResultRolledBack, []string{"ship:merged", "live:deployed", "soak:degraded", "ci:passed", "ship:merged", "live:deployed", "soak:healthy"}},
		{store.RunFailed, ResultAtRisk, []string{"ship:merged", "live:deployed", "soak:degraded", "back:failed"}},
		{store.RunFailed, ResultAtRisk, []string{"ship:merged"}},
		{store.RunFailed, ResultFailed, []string{"ci:failed"}},
		{store.RunFailed, ResultFailed, []string{"ci:passed", "ship:blocked"}},
		{store.RunSucceeded, ResultDone, nil},
		{store.RunCanceled, ResultCanceled, nil},
	} {
		run := releaseRun(t, a, fmt.Sprintf("r%d", i), tc.status, tc.steps...)
		if got, err := a.runResult(context.Background(), run); err != nil || got != tc.want {
			t.Errorf("%v: %s, want %s (%v)", tc.steps, got, tc.want, err)
		}
	}
}

func TestShippedRunIsSummarized(t *testing.T) {
	out, _ := json.Marshal(map[string]any{"headline": "Bill pages link to related bills", "changes": []string{"Each bill lists three others on its topic"},
		"checked": "Tests, review and a 10 minute soak.", "watch": ""})
	a, in := reportApp(t, string(out))
	run := releaseRun(t, a, "ship-it", store.RunSucceeded, "ship:merged", "live:deployed", "soak:healthy")
	run.PRURL = "https://github.com/o/r/pull/7"
	a.reportRun(context.Background(), run)
	if len(in.sent) != 1 {
		t.Fatalf("sent %+v", in.sent)
	}
	m := in.sent[0]
	if m.Title != "Shipped: Bill pages link to related bills" || !strings.Contains(m.Body, "• Each bill lists") || !strings.Contains(m.Body, "Checked: ") ||
		strings.Contains(m.Body, "Note:") || len(m.Actions) != 2 || m.Priority != 3 {
		t.Fatalf("%+v", m)
	}
}

func TestReportSurvivesAUselessModelReply(t *testing.T) {
	a, in := reportApp(t, "I cannot help with that")
	a.reportRun(context.Background(), releaseRun(t, a, "broken", store.RunFailed, "ship:merged"))
	if len(in.sent) != 1 || in.sent[0].Title != "Check production: broken" || in.sent[0].Priority != 5 {
		t.Fatalf("%+v", in.sent)
	}
}

func TestQuietResultsWaitForTheDigest(t *testing.T) {
	out, _ := json.Marshal(map[string]any{"headline": "Faster sitemap"})
	a, in := reportApp(t, string(out))
	ctx := context.Background()
	a.reportRun(ctx, releaseRun(t, a, "audit", store.RunSucceeded))
	if len(in.sent) != 0 {
		t.Fatalf("a run that shipped nothing was announced: %+v", in.sent)
	}
	a.reportRun(ctx, releaseRun(t, a, "sitemap", store.RunSucceeded, "ship:merged", "live:deployed", "soak:healthy"))
	in.sent = nil

	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(denver)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, denver)
	a.sendDigests(ctx, day.Add(7*time.Hour+59*time.Minute))
	if len(in.sent) != 0 {
		t.Fatal("digest before its time")
	}
	a.sendDigests(ctx, day.Add(8*time.Hour))
	a.sendDigests(ctx, day.Add(9*time.Hour))
	if len(in.sent) != 1 || !strings.Contains(in.sent[0].Title, "1 shipped, 1 other") || !strings.Contains(in.sent[0].Body, "Faster sitemap") {
		t.Fatalf("digest: %+v", in.sent)
	}
	// Nothing happened and nothing waits: no message.
	a.sendDigests(ctx, day.Add(72*time.Hour+9*time.Hour))
	if len(in.sent) != 1 {
		t.Fatalf("empty digest sent: %+v", in.sent[1:])
	}
}

func TestDigestOnlySuppressesImmediateAlerts(t *testing.T) {
	out, _ := json.Marshal(map[string]any{"headline": "Bill pages link to related bills"})
	a, in := reportApp(t, string(out))
	ctx := context.Background()
	a.Cfg.Current().Projects["sandbox"].Spec.Notify = &config.ProjectNotify{
		Summaries: false,
		Digest:    &config.DigestTime{At: "08:00", TZ: "America/Denver"},
	}
	a.reportRun(ctx, releaseRun(t, a, "ship-it", store.RunSucceeded, "ship:merged", "live:deployed", "soak:healthy"))
	if len(in.sent) != 0 {
		t.Fatalf("digest-only project sent immediate alert: %+v", in.sent)
	}
	denver, _ := time.LoadLocation("America/Denver")
	now := time.Now().In(denver)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, denver)
	a.sendDigests(ctx, day.Add(8*time.Hour))
	if len(in.sent) != 1 || !strings.Contains(in.sent[0].Title, "1 shipped") || !strings.Contains(in.sent[0].Body, "Bill pages link to related bills") {
		t.Fatalf("digest missing recorded run: %+v", in.sent)
	}
}

func TestCombinedEnvironmentDigest(t *testing.T) {
	out, _ := json.Marshal(map[string]any{"headline": "Faster bill search"})
	a, in := reportApp(t, string(out))
	ctx := context.Background()
	cfg := a.Cfg.Current()
	cfg.Env.Notify.Events = []string{}
	cfg.Env.Notify.Digest = &config.DigestTime{At: "08:00", TZ: "America/Denver"}
	cfg.Projects["sandbox"].Spec.Notify = nil
	cfg.Projects["other"] = &config.Project{
		Metadata: config.ProjectMetadata{Name: "other"},
		Spec:     config.ProjectSpec{Repo: "repo/sandbox"},
	}
	// Separate-digest project should not be duplicated into the combined digest.
	cfg.Projects["separate"] = &config.Project{
		Metadata: config.ProjectMetadata{Name: "separate"},
		Spec: config.ProjectSpec{
			Repo:   "repo/sandbox",
			Notify: &config.ProjectNotify{Digest: &config.DigestTime{At: "08:00", TZ: "America/Denver"}},
		},
	}

	a.reportRun(ctx, releaseRun(t, a, "search", store.RunSucceeded, "ship:merged", "live:deployed", "soak:healthy"))
	if len(in.sent) != 0 {
		t.Fatalf("combined digest-only mode sent immediate alert: %+v", in.sent)
	}
	nowMS := store.Now()
	hr, _ := json.Marshal(HoneReport{Summary: "Added 2 ready tasks", CommitSHA: "abc1234", Files: 2, At: nowMS})
	if err := a.Store.SetKV(ctx, kvHoneReport("other", nowMS), string(hr)); err != nil {
		t.Fatal(err)
	}
	sepRep, _ := json.Marshal(RunReport{RunID: "r-sep", Result: ResultShipped, Headline: "Separate item", At: nowMS})
	if err := a.Store.SetKV(ctx, kvReport("separate", "r-sep"), string(sepRep)); err != nil {
		t.Fatal(err)
	}

	denver, _ := time.LoadLocation("America/Denver")
	now := time.Now().In(denver)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, denver)
	a.sendDigests(ctx, day.Add(7*time.Hour+59*time.Minute))
	if len(in.sent) != 0 {
		t.Fatalf("digest sent early: %+v", in.sent)
	}
	a.sendDigests(ctx, day.Add(8*time.Hour))
	a.sendDigests(ctx, day.Add(9*time.Hour))
	if len(in.sent) != 2 {
		t.Fatalf("want 1 combined + 1 separate digest, got %d: %+v", len(in.sent), in.sent)
	}
	combined := in.sent[0]
	if combined.Title != "ai-flow today: 1 shipped, 1 honed" {
		t.Fatalf("unexpected combined title: %q", combined.Title)
	}
	for _, want := range []string{"sandbox (1 shipped):", "Faster bill search", "other (1 honed):", "Added 2 ready tasks"} {
		if !strings.Contains(combined.Body, want) {
			t.Errorf("combined body missing %q:\n%s", want, combined.Body)
		}
	}
	if strings.Contains(combined.Body, "Separate item") {
		t.Errorf("separate-digest project leaked into combined digest:\n%s", combined.Body)
	}
	if in.sent[1].Title != "separate today: 1 shipped" {
		t.Fatalf("unexpected separate digest: %+v", in.sent[1])
	}
}
