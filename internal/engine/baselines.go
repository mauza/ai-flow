package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// BaselineEntry stores the monotonic ratchet watermark for one numeric check
// output on a project (e.g. test_count with direction "min", or lint_warnings
// with direction "max").
type BaselineEntry struct {
	Value     float64 `json:"value"`
	Direction string  `json:"direction"` // "min" (must stay >= value) | "max" (must stay <= value)
	RunID     string  `json:"run_id,omitempty"`
	Node      string  `json:"node,omitempty"`
	UpdatedAt int64   `json:"updated_at"`
}

func baselinesKey(project string) string      { return "knowledge/" + project + "/baselines" }
func pendingBaselinesKey(runID string) string { return "run/" + runID + "/pending_baselines" }

// Baselines returns a project's stored metric ratchet baselines.
func (e *Engine) Baselines(ctx context.Context, project string) map[string]BaselineEntry {
	out := map[string]BaselineEntry{}
	if project == "" {
		return out
	}
	raw, err := e.store.GetKV(ctx, baselinesKey(project))
	if err != nil || raw == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("project baselines", "project", project, "err", err)
		}
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// SetBaselines replaces or merges entries into a project's stored baselines.
func (e *Engine) SetBaselines(ctx context.Context, project string, updates map[string]BaselineEntry) error {
	if project == "" {
		return nil
	}
	cur := e.Baselines(ctx, project)
	now := store.Now()
	for k, v := range updates {
		if v.Direction != "min" && v.Direction != "max" {
			return fmt.Errorf("baseline %q: direction must be min or max", k)
		}
		if v.UpdatedAt == 0 {
			v.UpdatedAt = now
		}
		cur[k] = v
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, baselinesKey(project), string(b))
}

// ResetBaselines clears a project's stored metric baselines.
func (e *Engine) ResetBaselines(ctx context.Context, project string) error {
	return e.store.WriteKV(ctx, nil, []string{baselinesKey(project)})
}

// applyRatchet enforces declared check ratchets when a check passes. If any
// ratcheted numeric output regressed below a "min" baseline or above a "max"
// baseline, it rewrites the visit outcome to "regressed" (if declared) or "fail"
// and appends the violation to summary. Improved values are staged per run and
// promoted into project baselines only when the run reaches $success.
func (e *Engine) applyRatchet(ctx context.Context, r *store.Run, n *resolve.Node, v *store.Visit) error {
	if n == nil || n.Type != flow.TypeCheck || len(n.Ratchet) == 0 || v.Outcome != "pass" || r.Project == "" {
		return nil
	}
	var outputs map[string]any
	if err := json.Unmarshal(v.Outputs, &outputs); err != nil || len(outputs) == 0 {
		return nil
	}
	base := e.Baselines(ctx, r.Project)
	pending := map[string]BaselineEntry{}
	if raw, err := e.store.GetKV(ctx, pendingBaselinesKey(r.ID)); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &pending)
	}
	var violations []string
	var staged bool
	for _, field := range slices.Sorted(maps.Keys(n.Ratchet)) {
		dir := n.Ratchet[field]
		val, ok := toFloat(outputs[field])
		if !ok {
			continue
		}
		if cur, has := base[field]; has {
			switch dir {
			case "min":
				if val < cur.Value {
					violations = append(violations, fmt.Sprintf("%s=%g < baseline %g (min)", field, val, cur.Value))
					continue
				}
			case "max":
				if val > cur.Value {
					violations = append(violations, fmt.Sprintf("%s=%g > baseline %g (max)", field, val, cur.Value))
					continue
				}
			}
			// Only stage when improved (or equal when establishing).
			if (dir == "min" && val > cur.Value) || (dir == "max" && val < cur.Value) {
				pending[field] = BaselineEntry{Value: val, Direction: dir, RunID: r.ID, Node: n.ID, UpdatedAt: store.Now()}
				staged = true
			}
		} else {
			pending[field] = BaselineEntry{Value: val, Direction: dir, RunID: r.ID, Node: n.ID, UpdatedAt: store.Now()}
			staged = true
		}
	}
	if len(violations) > 0 {
		targetOutcome := "fail"
		if slices.Contains(n.Outcomes, "regressed") {
			targetOutcome = "regressed"
		}
		summary := strings.TrimSpace(v.Summary)
		msg := "ratchet regression: " + strings.Join(violations, "; ")
		if summary != "" {
			summary += " — " + msg
		} else {
			summary = msg
		}
		if err := e.store.UpdateVisit(ctx, r.ID, v.Seq, map[string]any{"outcome": targetOutcome, "summary": summary}); err != nil {
			return err
		}
		v.Outcome, v.Summary = targetOutcome, summary
		e.event(ctx, r, v.Node, "ratchet", msg)
		return nil
	}
	if staged {
		if b, err := json.Marshal(pending); err == nil {
			_ = e.store.SetKV(ctx, pendingBaselinesKey(r.ID), string(b))
		}
	}
	return nil
}

// promoteBaselines commits a succeeded run's staged ratchet improvements into
// the project's baselines and cleans up the per-run staging key.
func (e *Engine) promoteBaselines(ctx context.Context, r *store.Run) {
	if r.Project == "" {
		return
	}
	key := pendingBaselinesKey(r.ID)
	raw, err := e.store.GetKV(ctx, key)
	if err != nil || raw == "" {
		return
	}
	var pending map[string]BaselineEntry
	if json.Unmarshal([]byte(raw), &pending) != nil || len(pending) == 0 {
		_ = e.store.WriteKV(ctx, nil, []string{key})
		return
	}
	cur := e.Baselines(ctx, r.Project)
	changed := false
	for k, cand := range pending {
		prev, exists := cur[k]
		if !exists || (cand.Direction == "min" && cand.Value > prev.Value) || (cand.Direction == "max" && cand.Value < prev.Value) {
			cur[k] = cand
			changed = true
		}
	}
	if changed {
		if b, err := json.Marshal(cur); err == nil {
			if err := e.store.SetKV(ctx, baselinesKey(r.Project), string(b)); err == nil {
				e.event(ctx, r, "", "ratchet", fmt.Sprintf("Promoted %d metric baseline(s)", len(pending)))
			}
		}
	}
	_ = e.store.WriteKV(ctx, nil, []string{key})
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
