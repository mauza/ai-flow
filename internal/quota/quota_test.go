package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/store"
)

type fakeUsage struct {
	usage map[string]store.ModelUsage
}

func (f fakeUsage) ModelUsageSince(_ context.Context, _ int64) (map[string]store.ModelUsage, error) {
	return f.usage, nil
}
func (f fakeUsage) PruneLLMUsage(_ context.Context, _ int64) error { return nil }

func testConfig(quotaURL string) *config.Config {
	return &config.Config{
		Env: config.Environment{
			Runs: config.Runs{MaxConcurrent: 3},
			LLM: config.LLM{
				Upstreams: map[string]config.Upstream{
					"home": {
						BaseURL: "http://litellm/v1",
						Quota: &config.UpstreamQuota{
							URL:         quotaURL,
							CostClasses: []string{"subscription"},
						},
					},
					"antigravity": {
						Type: config.UpstreamAntigravity,
						Quota: &config.UpstreamQuota{
							Tokens5h:     10_000_000,
							TokensWeekly: 100_000_000,
							CostClasses:  []string{"subscription"},
						},
					},
				},
			},
		},
		Catalog: config.Catalog{
			Planner: config.Planner{Model: "gpt-6.2-sol"},
			Models: map[string]*config.Model{
				"gpt-6.2-sol":        {Upstream: "home", Model: "gpt-6.2-sol", Cost: "subscription"},
				"gpt-6-luna":         {Upstream: "home", Model: "gpt-6-luna", Cost: "subscription"},
				"qwen3.8-27b":        {Upstream: "home", Model: "qwen3.8-27b", Cost: "local"},
				"gemini-4-neon-high": {Upstream: "antigravity", Model: "gemini-4-neon-high", Cost: "subscription"},
			},
		},
		Projects: map[string]*config.Project{
			"app": {
				Metadata: config.ProjectMetadata{Name: "app"},
				Spec: config.ProjectSpec{
					Allow: config.Allow{
						Models: []string{"gpt-6.2-sol", "gpt-6-luna", "qwen3.8-27b", "gemini-*"},
					},
				},
			},
		},
	}
}

func TestEvaluateWindowModes(t *testing.T) {
	now := int64(2_000_000)
	week := int64(604800)

	// 1. Hold at >=85%
	w := evaluateWindow(Window{
		Name: "weekly", Source: "upstream", WindowSeconds: week,
		UsedPercent: 86, ResetAt: now + week/2, ResetAfterSeconds: week / 2,
	}, 85, 65, 0.35, 3.0)
	if w.Mode != ModeHold || w.IntervalFactor != 0 {
		t.Fatalf("expected hold at 86%%, got %+v", w)
	}

	// 2. Boost when well under pace (6% used, 10% elapsed)
	w = evaluateWindow(Window{
		Name: "weekly", Source: "upstream", WindowSeconds: week,
		UsedPercent: 6, ResetAt: now + int64(float64(week)*0.9), ResetAfterSeconds: int64(float64(week) * 0.9),
	}, 85, 65, 0.35, 3.0)
	if w.Mode != ModeBoost || w.IntervalFactor >= 0.6 {
		t.Fatalf("expected strong boost at 6%%, got %+v", w)
	}

	// 3. End-of-window surplus boost (55% used with 80% elapsed -> plenty left before reset)
	w = evaluateWindow(Window{
		Name: "weekly", Source: "upstream", WindowSeconds: week,
		UsedPercent: 55, ResetAt: now + int64(float64(week)*0.2), ResetAfterSeconds: int64(float64(week) * 0.2),
	}, 85, 65, 0.35, 3.0)
	if w.Mode != ModeBoost || w.IntervalFactor >= 1.0 {
		t.Fatalf("expected end-of-window boost, got %+v", w)
	}

	// 4. Pace-based throttle (40% used when only 20% elapsed -> burning ~2.3x linear pace)
	w = evaluateWindow(Window{
		Name: "weekly", Source: "upstream", WindowSeconds: week,
		UsedPercent: 40, ResetAt: now + int64(float64(week)*0.8), ResetAfterSeconds: int64(float64(week) * 0.8),
	}, 85, 65, 0.35, 3.0)
	if w.Mode != ModeThrottle || w.IntervalFactor <= 1.25 {
		t.Fatalf("expected pace throttle, got %+v", w)
	}

	// 5. Utilization-based throttle (75% used even late in window)
	w = evaluateWindow(Window{
		Name: "weekly", Source: "upstream", WindowSeconds: week,
		UsedPercent: 75, ResetAt: now + int64(float64(week)*0.1), ResetAfterSeconds: int64(float64(week) * 0.1),
	}, 85, 65, 0.35, 3.0)
	if w.Mode != ModeThrottle || w.IntervalFactor <= 1.5 {
		t.Fatalf("expected utilization throttle at 75%%, got %+v", w)
	}
}

func TestTrackerPollAndScheduleDecision(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	payload := map[string]any{
		"provider":      "chatgpt",
		"plan_type":     "prolite",
		"allowed":       true,
		"limit_reached": false,
		"windows": []map[string]any{
			{
				"name":                "5h",
				"window_seconds":      18000,
				"used_percent":        10,
				"reset_after_seconds": 14400,
				"reset_at":            now.Unix() + 14400,
			},
			{
				"name":                "weekly",
				"window_seconds":      604800,
				"used_percent":        5,
				"reset_after_seconds": 550000,
				"reset_at":            now.Unix() + 550000,
			},
		},
	}
	failNext := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failNext {
			w.WriteHeader(502)
			w.Write([]byte("upstream down"))
			return
		}
		json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	tr := New(cfg, fakeUsage{usage: map[string]store.ModelUsage{
		"gpt-6.2-sol":        {TokensIn: 5_000_000, TokensOut: 200_000, TokensCached: 4_000_000, Calls: 10},
		"qwen3.8-27b":        {TokensIn: 99_000_000, TokensOut: 1_000_000, Calls: 50}, // local cost class: excluded!
		"gemini-4-neon-high": {TokensIn: 2_000_000, TokensOut: 100_000, Calls: 4},
	}})
	tr.now = func() time.Time { return now }
	tr.Refresh(context.Background())

	snap := tr.Snapshot(context.Background())
	if !snap.Enabled {
		t.Fatalf("expected pacing enabled")
	}
	if snap.Mode != ModeBoost {
		t.Fatalf("expected boost mode, got %s (%s)", snap.Mode, snap.Reason)
	}
	var homeSt *UpstreamStatus
	for i := range snap.Upstreams {
		if snap.Upstreams[i].Name == "home" {
			homeSt = &snap.Upstreams[i]
		}
	}
	if homeSt == nil {
		t.Fatalf("missing home upstream")
	}
	// qwen3.8-27b (local) must NOT be counted in home's subscription local tokens.
	if homeSt.Local.Tokens5h != 5_200_000 {
		t.Fatalf("expected home local 5h tokens=5200000 (excluding local qwen), got %d", homeSt.Local.Tokens5h)
	}

	// ScheduleDecision boosts concurrency above baseMaxActive=1 up to maxConcurrent=3.
	dec := tr.ScheduleDecision(context.Background(), "app", 1)
	if dec.Mode != ModeBoost || dec.Hold || dec.EffectiveMaxActive < 2 || dec.IntervalFactor >= 0.7 {
		t.Fatalf("expected boost decision, got %+v", dec)
	}

	// If 5h window hits 88% while weekly is 5%, conservative merge must HOLD!
	payload["windows"].([]map[string]any)[0]["used_percent"] = 88
	tr.Refresh(context.Background())
	dec = tr.ScheduleDecision(context.Background(), "app", 1)
	if dec.Mode != ModeHold || !dec.Hold || dec.EffectiveMaxActive != 0 {
		t.Fatalf("expected hold when 5h window >= 85%%, got %+v", dec)
	}

	// Planner summary should guide the planner toward antigravity while home is held.
	summary := tr.PlannerSummary(context.Background(), "app")
	if !strings.Contains(summary, "home") || !strings.Contains(summary, "antigravity") || !strings.Contains(summary, "Prefer models") {
		t.Fatalf("expected planner summary to advise shifting to headroom upstream, got:\n%s", summary)
	}

	// Stale fallback preserves last good probe on upstream error.
	failNext = true
	tr.Refresh(context.Background())
	snap = tr.Snapshot(context.Background())
	for _, u := range snap.Upstreams {
		if u.Name == "home" {
			if !u.Stale || u.Error == "" || len(u.Windows) != 2 {
				t.Fatalf("expected stale preserved probe, got %+v", u)
			}
		}
	}
}

func TestCooldownTriggersHold(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	cfg := testConfig("")
	tr := New(cfg, fakeUsage{})
	tr.now = func() time.Time { return now }

	tr.RecordLimitHit("antigravity", 10*time.Minute)
	snap := tr.Snapshot(context.Background())
	for _, u := range snap.Upstreams {
		if u.Name == "antigravity" && u.Mode != ModeHold {
			t.Fatalf("expected hold during cooldown, got %+v", u)
		}
	}
	// After cooldown expires, returns to boost (0% local usage).
	tr.now = func() time.Time { return now.Add(11 * time.Minute) }
	snap = tr.Snapshot(context.Background())
	for _, u := range snap.Upstreams {
		if u.Name == "antigravity" && u.Mode == ModeHold {
			t.Fatalf("expected cooldown cleared, got %+v", u)
		}
	}
}

func TestEffectiveInterval(t *testing.T) {
	if got := EffectiveInterval(4*time.Hour, 0.35); got != 84*time.Minute {
		t.Fatalf("4h * 0.35 = %v, want 1h24m", got)
	}
	if got := EffectiveInterval(4*time.Hour, 2.5); got != 10*time.Hour {
		t.Fatalf("4h * 2.5 = %v, want 10h", got)
	}
	if got := EffectiveInterval(20*time.Minute, 0.35); got != 15*time.Minute {
		t.Fatalf("floor should clamp to 15m, got %v", got)
	}
}
