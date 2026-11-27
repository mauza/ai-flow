package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/quota"
	"github.com/mauza/ai-flow/internal/store"
)

func TestSchedulerQuotaHoldAndBoost(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	usedPct := 90.0
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"provider":      "chatgpt",
			"plan_type":     "prolite",
			"allowed":       true,
			"limit_reached": false,
			"windows": []map[string]any{
				{
					"name":                "weekly",
					"window_seconds":      604800,
					"used_percent":        usedPct,
					"reset_after_seconds": 500000,
					"reset_at":            now.Unix() + 500000,
				},
			},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{
		Env: config.Environment{
			Runs: config.Runs{MaxConcurrent: 3},
			LLM: config.LLM{
				Upstreams: map[string]config.Upstream{
					"home": {
						BaseURL: "http://litellm/v1",
						Quota:   &config.UpstreamQuota{URL: srv.URL, CostClasses: []string{"subscription"}},
					},
				},
			},
		},
		Catalog: config.Catalog{
			Planner: config.Planner{Model: "gpt-6.2-sol"},
			Models: map[string]*config.Model{
				"gpt-6.2-sol": {Upstream: "home", Model: "gpt-6.2-sol", Cost: "subscription"},
			},
		},
		Projects: map[string]*config.Project{
			"demo": {
				Metadata: config.ProjectMetadata{Name: "demo"},
				Spec: config.ProjectSpec{
					Allow: config.Allow{Models: []string{"gpt-6.2-sol"}},
					Schedules: []config.ProjectSchedule{
						{
							Name:          "periodic",
							Every:         flow.Duration{Duration: 4 * time.Hour},
							MaxActiveRuns: 1,
							MaxOpenTasks:  5,
							MaxOpenPRs:    4,
							Task:          &config.ScheduleTask{Title: "Sweep"},
						},
					},
				},
			},
		},
	}

	qt := quota.New(cfg, st)
	qt.Refresh(ctx)
	a := &App{Cfg: cfg, Store: st, Hub: hub.New(), Quota: qt}

	// 1. At 90% (>=85% hold), non-forced trigger is skipped with quota hold.
	res, err := a.TriggerSchedule(ctx, "demo", "periodic", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastStatus != "skipped" || !strings.Contains(res.LastReason, "quota hold") {
		t.Fatalf("expected quota hold skip at 90%%, got %+v", res)
	}
	views := a.ListSchedules(ctx, "demo")
	if len(views) != 1 || views[0].PacingMode != quota.ModeHold || views[0].EffectiveMaxActiveRuns != 0 || views[0].NextDueAt != 0 {
		t.Fatalf("unexpected held schedule view: %+v", views)
	}

	// 2. Drop usage to 5% -> boost mode shortens interval and raises effective max active runs.
	usedPct = 5.0
	qt.Refresh(ctx)
	views = a.ListSchedules(ctx, "demo")
	if views[0].PacingMode != quota.ModeBoost {
		t.Fatalf("expected boost mode at 5%%, got %+v", views[0])
	}
	if views[0].EffectiveMaxActiveRuns < 2 {
		t.Fatalf("expected boosted effective_max_active_runs >= 2, got %d", views[0].EffectiveMaxActiveRuns)
	}
	if views[0].EffectiveEvery == views[0].Every {
		t.Fatalf("expected shortened effective_every under boost, still %s", views[0].EffectiveEvery)
	}

	// 3. Local usage recorded in SQLite shows up in Snapshot.
	if err := st.RecordLLMUsage(ctx, "gpt-6.2-sol", 1_500_000, 50_000, 1_000_000, "control-plane"); err != nil {
		t.Fatal(err)
	}
	snap := qt.Snapshot(ctx)
	if len(snap.Upstreams) != 1 || snap.Upstreams[0].Local.Tokens5h != 1_550_000 || snap.Upstreams[0].Local.TokensUncached5h != 550_000 {
		t.Fatalf("unexpected local usage in snapshot: %+v", snap.Upstreams)
	}
}
