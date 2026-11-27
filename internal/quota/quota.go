// Package quota tracks 5-hour and weekly LLM subscription quota across
// upstreams (live HTTP quota probes plus local rolling token ledgers) and
// computes pacing decisions (boost, normal, throttle, hold) for autonomous
// schedules and planner model selection.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/planner"
	"github.com/mauza/ai-flow/internal/store"
)

const (
	Window5h     = 5 * time.Hour
	WindowWeekly = 7 * 24 * time.Hour

	retentionWindow = 14 * 24 * time.Hour
)

// Mode describes the pacing posture for a window, upstream, or schedule.
type Mode string

const (
	ModeBoost    Mode = "boost"
	ModeNormal   Mode = "normal"
	ModeThrottle Mode = "throttle"
	ModeHold     Mode = "hold"
)

// UsageReader reads rolling token totals per catalog model from the store.
type UsageReader interface {
	ModelUsageSince(ctx context.Context, sinceMS int64) (map[string]store.ModelUsage, error)
	PruneLLMUsage(ctx context.Context, beforeMS int64) error
}

// Window describes utilization and burn-rate pacing for one 5h or weekly window.
type Window struct {
	Name              string  `json:"name"`
	Source            string  `json:"source"` // upstream | local
	WindowSeconds     int64   `json:"window_seconds"`
	UsedPercent       float64 `json:"used_percent"`
	RemainingPercent  float64 `json:"remaining_percent"`
	ResetAt           int64   `json:"reset_at,omitempty"`
	ResetAfterSeconds int64   `json:"reset_after_seconds,omitempty"`
	TokensUsed        int64   `json:"tokens_used,omitempty"`
	TokensLimit       int64   `json:"tokens_limit,omitempty"`
	ElapsedPercent    float64 `json:"elapsed_percent,omitempty"`
	PaceRatio         float64 `json:"pace_ratio,omitempty"`
	Mode              Mode    `json:"mode"`
	IntervalFactor    float64 `json:"interval_factor"`
}

// LocalUsage summarizes ai-flow's own recorded token consumption on an upstream.
type LocalUsage struct {
	Tokens5h         int64 `json:"tokens_5h"`
	TokensUncached5h int64 `json:"tokens_uncached_5h"`
	TokensWeekly     int64 `json:"tokens_weekly"`
	TokensUncached7d int64 `json:"tokens_uncached_weekly"`
	Calls5h          int64 `json:"calls_5h"`
	CallsWeekly      int64 `json:"calls_weekly"`
}

// UpstreamStatus is the live quota and pacing state for one configured upstream.
type UpstreamStatus struct {
	Name           string     `json:"name"`
	Provider       string     `json:"provider,omitempty"`
	PlanType       string     `json:"plan_type,omitempty"`
	Allowed        bool       `json:"allowed"`
	LimitReached   bool       `json:"limit_reached,omitempty"`
	CooldownUntil  int64      `json:"cooldown_until,omitempty"`
	FetchedAt      int64      `json:"fetched_at,omitempty"`
	Stale          bool       `json:"stale,omitempty"`
	Error          string     `json:"error,omitempty"`
	Windows        []Window   `json:"windows"`
	Local          LocalUsage `json:"local"`
	Mode           Mode       `json:"mode"`
	IntervalFactor float64    `json:"interval_factor"`
	ConcurrencyCap int        `json:"concurrency_cap"`
	Reason         string     `json:"reason,omitempty"`
}

// Snapshot is the full control-plane view of subscription quotas and pacing.
type Snapshot struct {
	Enabled         bool             `json:"enabled"`
	UpdatedAt       int64            `json:"updated_at"`
	HoldPercent     float64          `json:"hold_percent"`
	ThrottlePercent float64          `json:"throttle_percent"`
	Mode            Mode             `json:"mode"`
	IntervalFactor  float64          `json:"interval_factor"`
	Reason          string           `json:"reason,omitempty"`
	Upstreams       []UpstreamStatus `json:"upstreams"`
}

// Decision tells the scheduler how to pace one project's schedule.
type Decision struct {
	Enabled            bool    `json:"enabled"`
	Mode               Mode    `json:"mode"`
	Hold               bool    `json:"hold"`
	IntervalFactor     float64 `json:"interval_factor"`
	EffectiveMaxActive int     `json:"effective_max_active"`
	Reason             string  `json:"reason,omitempty"`
	PrimaryUpstream    string  `json:"primary_upstream,omitempty"`
}

type probeWindow struct {
	Name              string  `json:"name"`
	WindowSeconds     int64   `json:"window_seconds"`
	UsedPercent       float64 `json:"used_percent"`
	ResetAfterSeconds int64   `json:"reset_after_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

type probePayload struct {
	Provider     string        `json:"provider"`
	PlanType     string        `json:"plan_type"`
	Allowed      *bool         `json:"allowed"`
	LimitReached bool          `json:"limit_reached"`
	FetchedAt    int64         `json:"fetched_at"`
	Stale        bool          `json:"stale"`
	Error        string        `json:"error"`
	Windows      []probeWindow `json:"windows"`
}

type probeState struct {
	payload   *probePayload
	fetchedAt time.Time
	err       string
	stale     bool
}

// Tracker polls upstream quota endpoints, aggregates local 5h/weekly token
// usage, tracks upstream limit cooldowns, and computes pacing decisions.
type Tracker struct {
	cfg  *config.Config
	st   UsageReader
	http *http.Client
	now  func() time.Time

	mu        sync.RWMutex
	probes    map[string]*probeState
	cooldowns map[string]time.Time
}

func New(cfg *config.Config, st UsageReader) *Tracker {
	return &Tracker{
		cfg:       cfg,
		st:        st,
		http:      &http.Client{Timeout: 10 * time.Second},
		now:       time.Now,
		probes:    map[string]*probeState{},
		cooldowns: map[string]time.Time{},
	}
}

// Run polls upstream quota endpoints periodically and prunes old usage rows.
func (t *Tracker) Run(ctx context.Context) {
	t.Refresh(ctx)
	interval := t.cfg.Current().Env.LLM.Pacing.EffectivePollInterval()
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	poll := time.NewTicker(interval)
	defer poll.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			t.Refresh(ctx)
		case <-prune.C:
			if t.st != nil {
				cutoff := t.now().Add(-retentionWindow).UnixMilli()
				if err := t.st.PruneLLMUsage(ctx, cutoff); err != nil {
					slog.Warn("prune llm usage", "err", err)
				}
			}
		}
	}
}

// Refresh polls every upstream that configures a quota URL.
func (t *Tracker) Refresh(ctx context.Context) {
	cfg := t.cfg.Current()
	for name, up := range cfg.Env.LLM.Upstreams {
		if up.Quota == nil || strings.TrimSpace(up.Quota.URL) == "" {
			continue
		}
		t.refreshUpstream(ctx, name, up)
	}
}

func (t *Tracker) refreshUpstream(ctx context.Context, name string, up config.Upstream) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, "GET", up.Quota.URL, nil)
	if err != nil {
		t.recordProbeError(name, err)
		return
	}
	req.Header.Set("Accept", "application/json")
	keyEnv := up.Quota.APIKeyEnv
	if keyEnv == "" {
		keyEnv = up.APIKeyEnv
	}
	if key := config.Secret(keyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		t.recordProbeError(name, err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		t.recordProbeError(name, err)
		return
	}
	if resp.StatusCode >= 300 {
		t.recordProbeError(name, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	var payload probePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.recordProbeError(name, fmt.Errorf("invalid quota JSON: %w", err))
		return
	}
	t.mu.Lock()
	t.probes[name] = &probeState{
		payload:   &payload,
		fetchedAt: t.now(),
		err:       payload.Error,
		stale:     payload.Stale,
	}
	t.mu.Unlock()
}

func (t *Tracker) recordProbeError(name string, err error) {
	slog.Warn("quota poll failed", "upstream", name, "err", err)
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.probes[name]
	if prev != nil && prev.payload != nil {
		copyState := *prev
		copyState.stale = true
		copyState.err = err.Error()
		t.probes[name] = &copyState
		return
	}
	t.probes[name] = &probeState{
		fetchedAt: t.now(),
		err:       err.Error(),
		stale:     true,
	}
}

// RecordLimitHit marks an upstream as recently rate/quota-limited so pacing
// backs off immediately even between quota polls.
func (t *Tracker) RecordLimitHit(upstream string, retryAfter time.Duration) {
	if upstream == "" {
		return
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	if retryAfter < time.Minute {
		retryAfter = time.Minute
	}
	if retryAfter > 2*time.Hour {
		retryAfter = 2 * time.Hour
	}
	until := t.now().Add(retryAfter)
	t.mu.Lock()
	if prev, ok := t.cooldowns[upstream]; !ok || until.After(prev) {
		t.cooldowns[upstream] = until
	}
	t.mu.Unlock()
}

// Snapshot returns the current quota and pacing view across all upstreams.
func (t *Tracker) Snapshot(ctx context.Context) *Snapshot {
	cfg := t.cfg.Current()
	now := t.now()
	pacing := cfg.Env.LLM.Pacing

	hasQuota := false
	for _, up := range cfg.Env.LLM.Upstreams {
		if up.Quota.Configured() {
			hasQuota = true
			break
		}
	}
	enabled := pacing.IsEnabled(hasQuota)

	usage5h, usage7d := t.loadLocalUsage(ctx, now)

	// Map upstream -> models that belong to its counted cost classes.
	names := make([]string, 0, len(cfg.Env.LLM.Upstreams))
	for name := range cfg.Env.LLM.Upstreams {
		names = append(names, name)
	}
	sort.Strings(names)

	snap := &Snapshot{
		Enabled:         enabled,
		UpdatedAt:       now.UnixMilli(),
		HoldPercent:     pacing.EffectiveHoldPercent(),
		ThrottlePercent: pacing.EffectiveThrottlePercent(),
		Mode:            ModeNormal,
		IntervalFactor:  1.0,
		Upstreams:       make([]UpstreamStatus, 0, len(names)),
	}

	byName := make(map[string]UpstreamStatus, len(names))
	for _, name := range names {
		up := cfg.Env.LLM.Upstreams[name]
		st := t.evalUpstream(cfg, name, up, usage5h, usage7d, now)
		snap.Upstreams = append(snap.Upstreams, st)
		byName[name] = st
	}

	if !enabled {
		return snap
	}

	// Overall posture follows the planner's upstream when known, otherwise the
	// most constrained configured upstream.
	primary := ""
	if pm := cfg.Catalog.Models[cfg.Catalog.Planner.Model]; pm != nil {
		primary = pm.Upstream
	}
	if st, ok := byName[primary]; ok && cfg.Env.LLM.Upstreams[primary].Quota.Configured() {
		snap.Mode = st.Mode
		snap.IntervalFactor = st.IntervalFactor
		snap.Reason = st.Reason
		return snap
	}
	for _, st := range snap.Upstreams {
		if !cfg.Env.LLM.Upstreams[st.Name].Quota.Configured() {
			continue
		}
		if modeSeverity(st.Mode) > modeSeverity(snap.Mode) || (st.Mode == ModeBoost && snap.Mode == ModeNormal) {
			snap.Mode = st.Mode
			snap.IntervalFactor = st.IntervalFactor
			snap.Reason = st.Reason
		}
	}
	return snap
}

// ScheduleDecision computes how a project's autonomous schedule should pace
// its interval and active-run concurrency given live quota headroom.
func (t *Tracker) ScheduleDecision(ctx context.Context, project string, baseMaxActive int) Decision {
	if baseMaxActive < 1 {
		baseMaxActive = 1
	}
	cfg := t.cfg.Current()
	snap := t.Snapshot(ctx)
	if !snap.Enabled {
		return Decision{
			Enabled:            false,
			Mode:               ModeNormal,
			IntervalFactor:     1.0,
			EffectiveMaxActive: baseMaxActive,
		}
	}

	primary := t.primaryUpstream(cfg, project)
	if primary == "" {
		return Decision{
			Enabled:            false,
			Mode:               ModeNormal,
			IntervalFactor:     1.0,
			EffectiveMaxActive: baseMaxActive,
		}
	}
	var st *UpstreamStatus
	for i := range snap.Upstreams {
		if snap.Upstreams[i].Name == primary {
			st = &snap.Upstreams[i]
			break
		}
	}
	if st == nil || !cfg.Env.LLM.Upstreams[primary].Quota.Configured() {
		return Decision{
			Enabled:            false,
			Mode:               ModeNormal,
			IntervalFactor:     1.0,
			EffectiveMaxActive: baseMaxActive,
			PrimaryUpstream:    primary,
		}
	}

	dec := Decision{
		Enabled:            true,
		Mode:               st.Mode,
		IntervalFactor:     st.IntervalFactor,
		EffectiveMaxActive: baseMaxActive,
		Reason:             st.Reason,
		PrimaryUpstream:    primary,
	}
	maxGlobal := cfg.Env.Runs.MaxConcurrent
	if maxGlobal < 1 {
		maxGlobal = 1
	}
	switch st.Mode {
	case ModeHold:
		dec.Hold = true
		dec.EffectiveMaxActive = 0
		dec.IntervalFactor = 0
	case ModeThrottle:
		dec.EffectiveMaxActive = min(baseMaxActive, 1)
	case ModeBoost:
		boosted := max(baseMaxActive, st.ConcurrencyCap)
		if boosted > maxGlobal {
			boosted = maxGlobal
		}
		dec.EffectiveMaxActive = boosted
	}
	return dec
}

// PlannerSummary formats a concise live quota report for the planner's system
// prompt so it can shift steps toward the upstream with more headroom.
func (t *Tracker) PlannerSummary(ctx context.Context, project string) string {
	cfg := t.cfg.Current()
	snap := t.Snapshot(ctx)
	if !snap.Enabled {
		return ""
	}
	proj := cfg.Projects[project]
	menu := planner.ProjectEligibility(cfg, proj)
	allowedUpstreams := map[string][]string{}
	for _, mname := range menu.AllowedModels {
		m := cfg.Catalog.Models[mname]
		if m == nil {
			continue
		}
		up := cfg.Env.LLM.Upstreams[m.Upstream]
		if !up.Quota.Configured() {
			continue
		}
		if !matchesCostClass(m.Cost, up.Quota.EffectiveCostClasses()) {
			continue
		}
		allowedUpstreams[m.Upstream] = append(allowedUpstreams[m.Upstream], mname)
	}
	if len(allowedUpstreams) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Current subscription quota status across allowed upstreams (hold reserve at ")
	fmt.Fprintf(&sb, "%.0f%%):\n", snap.HoldPercent)
	hasTight := false
	hasHeadroom := false
	for _, st := range snap.Upstreams {
		models, ok := allowedUpstreams[st.Name]
		if !ok {
			continue
		}
		if st.Mode == ModeThrottle || st.Mode == ModeHold {
			hasTight = true
		} else {
			hasHeadroom = true
		}
		label := st.Name
		if st.PlanType != "" {
			label += " (" + st.PlanType + ")"
		}
		var parts []string
		for _, w := range st.Windows {
			s := fmt.Sprintf("%s %.0f%% used", w.Name, w.UsedPercent)
			if w.ResetAfterSeconds > 0 {
				s += " (resets in " + formatDurationShort(time.Duration(w.ResetAfterSeconds)*time.Second) + ")"
			} else if w.TokensLimit > 0 {
				s += fmt.Sprintf(" (%s/%s tokens)", formatTokens(w.TokensUsed), formatTokens(w.TokensLimit))
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			parts = append(parts, fmt.Sprintf("local usage: %s in 5h, %s in 7d", formatTokens(st.Local.Tokens5h), formatTokens(st.Local.TokensWeekly)))
		}
		sample := models
		if len(sample) > 4 {
			sample = sample[:4]
		}
		fmt.Fprintf(&sb, "- `%s` [%s]: %s. Example models: %s.\n", label, st.Mode, strings.Join(parts, "; "), strings.Join(sample, ", "))
	}
	if hasTight && hasHeadroom {
		sb.WriteString("\nPrefer models from an upstream in `boost` or `normal` mode over a `throttle` or `hold` upstream for heavy implementation, review, and QA steps (still using a different model for independent review).\n")
	} else if len(allowedUpstreams) > 1 {
		sb.WriteString("\nWhen one upstream becomes tight (`throttle` or `hold`), prefer allowed models on an upstream with more headroom.\n")
	}
	return sb.String()
}

func (t *Tracker) primaryUpstream(cfg *config.Config, project string) string {
	if pm := cfg.Catalog.Models[cfg.Catalog.Planner.Model]; pm != nil && cfg.Env.LLM.Upstreams[pm.Upstream].Quota.Configured() {
		return pm.Upstream
	}
	proj := cfg.Projects[project]
	menu := planner.ProjectEligibility(cfg, proj)
	for _, name := range menu.AllowedModels {
		if m := cfg.Catalog.Models[name]; m != nil && cfg.Env.LLM.Upstreams[m.Upstream].Quota.Configured() {
			return m.Upstream
		}
	}
	return ""
}

func (t *Tracker) loadLocalUsage(ctx context.Context, now time.Time) (map[string]store.ModelUsage, map[string]store.ModelUsage) {
	if t.st == nil {
		return nil, nil
	}
	u5, err := t.st.ModelUsageSince(ctx, now.Add(-Window5h).UnixMilli())
	if err != nil {
		slog.Warn("quota local 5h usage query failed", "err", err)
	}
	u7, err := t.st.ModelUsageSince(ctx, now.Add(-WindowWeekly).UnixMilli())
	if err != nil {
		slog.Warn("quota local 7d usage query failed", "err", err)
	}
	return u5, u7
}

func (t *Tracker) evalUpstream(cfg *config.Config, name string, up config.Upstream, usage5h, usage7d map[string]store.ModelUsage, now time.Time) UpstreamStatus {
	pacing := cfg.Env.LLM.Pacing
	holdPct := pacing.EffectiveHoldPercent()
	throttlePct := pacing.EffectiveThrottlePercent()
	minFactor := pacing.EffectiveMinIntervalFactor()
	maxFactor := pacing.EffectiveMaxIntervalFactor()
	maxBoostConc := pacing.EffectiveMaxBoostConcurrency()

	st := UpstreamStatus{
		Name:           name,
		Allowed:        true,
		Windows:        []Window{},
		Mode:           ModeNormal,
		IntervalFactor: 1.0,
		ConcurrencyCap: 1,
	}

	// Aggregate local rolling 5h/7d tokens for models on this upstream matching CostClasses.
	classes := up.Quota.EffectiveCostClasses()
	for modelName, m := range cfg.Catalog.Models {
		if m.Upstream != name || !matchesCostClass(m.Cost, classes) {
			continue
		}
		if u, ok := usage5h[modelName]; ok {
			st.Local.Tokens5h += u.TokensIn + u.TokensOut
			st.Local.TokensUncached5h += u.Uncached()
			st.Local.Calls5h += u.Calls
		}
		if u, ok := usage7d[modelName]; ok {
			st.Local.TokensWeekly += u.TokensIn + u.TokensOut
			st.Local.TokensUncached7d += u.Uncached()
			st.Local.CallsWeekly += u.Calls
		}
	}

	t.mu.RLock()
	ps := t.probes[name]
	cd := t.cooldowns[name]
	t.mu.RUnlock()

	if cd.After(now) {
		st.CooldownUntil = cd.UnixMilli()
	}

	// 1. Live upstream probe windows (authoritative account-wide usage).
	if ps != nil {
		st.FetchedAt = ps.fetchedAt.UnixMilli()
		st.Stale = ps.stale
		st.Error = ps.err
		if ps.payload != nil {
			p := ps.payload
			st.Provider = p.Provider
			st.PlanType = p.PlanType
			if p.Allowed != nil {
				st.Allowed = *p.Allowed
			}
			st.LimitReached = p.LimitReached
			for _, pw := range p.Windows {
				if pw.WindowSeconds <= 0 {
					continue
				}
				resetAfter := pw.ResetAfterSeconds
				if pw.ResetAt > 0 {
					resetAfter = max(0, pw.ResetAt-now.Unix())
				}
				w := evaluateWindow(Window{
					Name:              pw.Name,
					Source:            "upstream",
					WindowSeconds:     pw.WindowSeconds,
					UsedPercent:       clampFloat(pw.UsedPercent, 0, 100),
					RemainingPercent:  clampFloat(100-pw.UsedPercent, 0, 100),
					ResetAt:           pw.ResetAt,
					ResetAfterSeconds: resetAfter,
				}, holdPct, throttlePct, minFactor, maxFactor)
				st.Windows = append(st.Windows, w)
			}
		}
	}

	// 2. Local rolling token budget windows (when configured on the upstream).
	if up.Quota != nil {
		if up.Quota.Tokens5h > 0 {
			pct := 100 * float64(st.Local.Tokens5h) / float64(up.Quota.Tokens5h)
			w := evaluateWindow(Window{
				Name:             "5h-local",
				Source:           "local",
				WindowSeconds:    int64(Window5h.Seconds()),
				UsedPercent:      round1(clampFloat(pct, 0, 100)),
				RemainingPercent: round1(clampFloat(100-pct, 0, 100)),
				TokensUsed:       st.Local.Tokens5h,
				TokensLimit:      up.Quota.Tokens5h,
			}, holdPct, throttlePct, minFactor, maxFactor)
			st.Windows = append(st.Windows, w)
		}
		if up.Quota.TokensWeekly > 0 {
			pct := 100 * float64(st.Local.TokensWeekly) / float64(up.Quota.TokensWeekly)
			w := evaluateWindow(Window{
				Name:             "weekly-local",
				Source:           "local",
				WindowSeconds:    int64(WindowWeekly.Seconds()),
				UsedPercent:      round1(clampFloat(pct, 0, 100)),
				RemainingPercent: round1(clampFloat(100-pct, 0, 100)),
				TokensUsed:       st.Local.TokensWeekly,
				TokensLimit:      up.Quota.TokensWeekly,
			}, holdPct, throttlePct, minFactor, maxFactor)
			st.Windows = append(st.Windows, w)
		}
	}

	// Combine windows into upstream pacing posture.
	if st.LimitReached || !st.Allowed {
		st.Mode = ModeHold
		st.IntervalFactor = 0
		st.ConcurrencyCap = 0
		st.Reason = fmt.Sprintf("%s quota limit reached upstream", name)
		return st
	}
	if st.CooldownUntil > 0 {
		rem := time.Until(time.UnixMilli(st.CooldownUntil)).Round(time.Second)
		if rem < time.Second {
			rem = time.Second
		}
		st.Mode = ModeHold
		st.IntervalFactor = 0
		st.ConcurrencyCap = 0
		st.Reason = fmt.Sprintf("%s rate-limit cooldown (%s remaining)", name, formatDurationShort(rem))
		return st
	}
	if len(st.Windows) == 0 {
		return st
	}

	// Conservative merge: any Hold => Hold; else any Throttle => Throttle (max factor);
	// else if any Boost => Boost (min factor), with concurrency boost proportional to headroom.
	worstHold := (*Window)(nil)
	worstThrottle := (*Window)(nil)
	bestBoost := (*Window)(nil)
	for i := range st.Windows {
		w := &st.Windows[i]
		switch w.Mode {
		case ModeHold:
			if worstHold == nil || w.UsedPercent > worstHold.UsedPercent {
				worstHold = w
			}
		case ModeThrottle:
			if worstThrottle == nil || w.IntervalFactor > worstThrottle.IntervalFactor {
				worstThrottle = w
			}
		case ModeBoost:
			if bestBoost == nil || w.IntervalFactor < bestBoost.IntervalFactor {
				bestBoost = w
			}
		}
	}

	switch {
	case worstHold != nil:
		st.Mode = ModeHold
		st.IntervalFactor = 0
		st.ConcurrencyCap = 0
		st.Reason = fmt.Sprintf("%s %s at %.0f%% (>= %.0f%% reserve)", name, worstHold.Name, worstHold.UsedPercent, holdPct)
	case worstThrottle != nil:
		st.Mode = ModeThrottle
		st.IntervalFactor = round2(worstThrottle.IntervalFactor)
		st.ConcurrencyCap = 1
		st.Reason = fmt.Sprintf("%s %s at %.0f%% (throttling %.1fx slower)", name, worstThrottle.Name, worstThrottle.UsedPercent, st.IntervalFactor)
	case bestBoost != nil:
		st.Mode = ModeBoost
		st.IntervalFactor = round2(bestBoost.IntervalFactor)
		// Scale concurrency boost from 2 (moderate surplus) up to maxBoostConc (strong surplus).
		conc := 2
		if st.IntervalFactor <= minFactor+0.15 {
			conc = maxBoostConc
		}
		if conc > maxBoostConc {
			conc = maxBoostConc
		}
		if conc < 1 {
			conc = 1
		}
		st.ConcurrencyCap = conc
		st.Reason = fmt.Sprintf("%s %s at %.0f%% (boosting %.1fx faster, up to %d concurrent)", name, bestBoost.Name, bestBoost.UsedPercent, 1/st.IntervalFactor, conc)
	default:
		st.Mode = ModeNormal
		st.IntervalFactor = 1.0
		st.ConcurrencyCap = 1
	}
	return st
}

// evaluateWindow scores a single 5h or weekly window into hold/throttle/normal/boost.
func evaluateWindow(w Window, holdPct, throttlePct, minFactor, maxFactor float64) Window {
	w.UsedPercent = round1(w.UsedPercent)
	w.RemainingPercent = round1(clampFloat(100-w.UsedPercent, 0, 100))

	if w.UsedPercent >= holdPct {
		w.Mode = ModeHold
		w.IntervalFactor = 0
		return w
	}

	// Fixed-reset upstream windows have an elapsed fraction and a linear burn target
	// that reaches holdPct right as the window resets.
	hasReset := w.Source == "upstream" && w.WindowSeconds > 0 && w.ResetAt > 0
	if hasReset {
		elapsed := 1.0 - float64(w.ResetAfterSeconds)/float64(w.WindowSeconds)
		elapsed = clampFloat(elapsed, 0.0, 1.0)
		w.ElapsedPercent = round1(elapsed * 100)
		// Expected utilization along a smooth curve targeting holdPct at reset,
		// with a 15% floor early in a fresh window so small initial bursts don't
		// immediately trigger heavy throttling.
		expectedPct := math.Max(elapsed*holdPct, 15.0)
		pace := w.UsedPercent / expectedPct
		w.PaceRatio = round2(pace)

		// Throttle if above absolute throttlePct OR burning noticeably ahead of linear pace
		// (once usage is non-trivial, >= 20%).
		if w.UsedPercent >= throttlePct || (w.UsedPercent >= 20 && pace > 1.15) {
			w.Mode = ModeThrottle
			// Severity 0..1 from whichever signal is stronger: utilization toward holdPct,
			// or pace ratio above 1.15 toward 2.2.
			utilSev := 0.0
			if holdPct > throttlePct && w.UsedPercent > throttlePct {
				utilSev = clampFloat((w.UsedPercent-throttlePct)/(holdPct-throttlePct), 0, 1)
			}
			paceSev := 0.0
			if w.UsedPercent >= 20 && pace > 1.15 {
				paceSev = clampFloat((pace-1.15)/1.05, 0, 1)
			}
			sev := math.Max(utilSev, paceSev)
			w.IntervalFactor = round2(1.25 + sev*(maxFactor-1.25))
			return w
		}

		// Boost when well under linear pace OR late in the window with lots of unused quota
		// ("use it or lose it" before reset).
		endOfWindowSurplus := elapsed >= 0.65 && w.UsedPercent <= holdPct-15
		underPaceSurplus := pace <= 0.65 && w.UsedPercent < throttlePct-15
		if endOfWindowSurplus || underPaceSurplus {
			w.Mode = ModeBoost
			// Surplus 0..1: stronger when utilization is very low or reset is approaching with headroom.
			headroom := clampFloat((holdPct-w.UsedPercent)/holdPct, 0, 1)
			urgency := 0.0
			if elapsed >= 0.5 {
				urgency = clampFloat((elapsed-0.5)/0.5, 0, 1) * headroom
			}
			strength := math.Max(headroom*0.85, urgency)
			w.IntervalFactor = round2(1.0 - strength*(1.0-minFactor))
			if w.IntervalFactor < minFactor {
				w.IntervalFactor = minFactor
			}
			return w
		}

		w.Mode = ModeNormal
		w.IntervalFactor = 1.0
		return w
	}

	// Rolling local token window (no fixed reset cliff): purely utilization-based.
	if w.UsedPercent >= throttlePct {
		w.Mode = ModeThrottle
		sev := 0.0
		if holdPct > throttlePct {
			sev = clampFloat((w.UsedPercent-throttlePct)/(holdPct-throttlePct), 0, 1)
		}
		w.IntervalFactor = round2(1.25 + sev*(maxFactor-1.25))
		return w
	}
	if w.UsedPercent <= 35 {
		w.Mode = ModeBoost
		strength := clampFloat((35-w.UsedPercent)/35, 0, 1)
		w.IntervalFactor = round2(0.8 - strength*(0.8-minFactor))
		if w.IntervalFactor < minFactor {
			w.IntervalFactor = minFactor
		}
		return w
	}
	w.Mode = ModeNormal
	w.IntervalFactor = 1.0
	return w
}

// EffectiveInterval applies a pacing factor to a schedule's configured interval,
// keeping the result within sane bounds (at least 15m, at most 7d).
func EffectiveInterval(base time.Duration, factor float64) time.Duration {
	if base <= 0 || factor <= 0 || factor == 1.0 {
		return base
	}
	scaled := time.Duration(float64(base) * factor)
	minFloor := min(15*time.Minute, base)
	if scaled < minFloor {
		scaled = minFloor
	}
	if maxCeil := 7 * 24 * time.Hour; scaled > maxCeil {
		scaled = maxCeil
	}
	return scaled.Round(time.Minute)
}

func matchesCostClass(cost string, classes []string) bool {
	if slices.Contains(classes, "*") {
		return true
	}
	return slices.Contains(classes, cost)
}

func modeSeverity(m Mode) int {
	switch m {
	case ModeHold:
		return 3
	case ModeThrottle:
		return 2
	case ModeNormal:
		return 1
	case ModeBoost:
		return 0
	default:
		return 1
	}
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatDurationShort(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", max(1, mins))
	}
}
