package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/store"
)

// Ledger statuses for persistent per-project improvement memory.
const (
	LedgerProposed = "proposed"
	LedgerShipped  = "shipped"
	LedgerReverted = "reverted"
	LedgerRejected = "rejected"
	LedgerWontFix  = "wont_fix"
)

// LedgerEntry records one discovered, attempted, shipped, or dismissed
// improvement item for a project so recurring audit loops never repeat stale
// or rejected proposals.
type LedgerEntry struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Category  string `json:"category,omitempty"`
	Status    string `json:"status"` // proposed | shipped | reverted | rejected | wont_fix
	Summary   string `json:"summary,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	UpdatedAt int64  `json:"updated_at"`
}

const (
	ledgerMaxEntries   = 200
	ledgerContextLimit = 6000
)

func ledgerKey(project string) string { return "knowledge/" + project + "/ledger" }

// Ledger returns a project's improvement ledger, newest updated first.
func (e *Engine) Ledger(ctx context.Context, project string) []LedgerEntry {
	if project == "" {
		return nil
	}
	raw, err := e.store.GetKV(ctx, ledgerKey(project))
	if err != nil || raw == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("project ledger", "project", project, "err", err)
		}
		return nil
	}
	var out []LedgerEntry
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// UpsertLedger merges entries by normalized key into the project's ledger.
func (e *Engine) UpsertLedger(ctx context.Context, project string, entries ...LedgerEntry) error {
	if project == "" || len(entries) == 0 {
		return nil
	}
	existing := e.Ledger(ctx, project)
	byKey := map[string]LedgerEntry{}
	for _, it := range existing {
		k := normalizeLedgerKey(it.Key)
		if k != "" {
			it.Key = k
			byKey[k] = it
		}
	}
	now := store.Now()
	for _, it := range entries {
		k := normalizeLedgerKey(it.Key)
		if k == "" {
			k = normalizeLedgerKey(it.Title)
		}
		if k == "" {
			continue
		}
		prev := byKey[k]
		it.Key = k
		if strings.TrimSpace(it.Title) == "" {
			it.Title = prev.Title
		}
		if strings.TrimSpace(it.Category) == "" {
			it.Category = prev.Category
		}
		if strings.TrimSpace(it.Summary) == "" {
			it.Summary = prev.Summary
		}
		if strings.TrimSpace(it.Evidence) == "" {
			it.Evidence = prev.Evidence
		}
		if strings.TrimSpace(it.RunID) == "" {
			it.RunID = prev.RunID
		}
		if strings.TrimSpace(it.PRURL) == "" {
			it.PRURL = prev.PRURL
		}
		it.Status = normalizeLedgerStatus(it.Status, prev.Status)
		if it.UpdatedAt == 0 {
			it.UpdatedAt = now
		}
		byKey[k] = it
	}
	merged := make([]LedgerEntry, 0, len(byKey))
	for _, it := range byKey {
		merged = append(merged, it)
	}
	slices.SortFunc(merged, func(a, b LedgerEntry) int {
		if a.UpdatedAt != b.UpdatedAt {
			if a.UpdatedAt > b.UpdatedAt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	if len(merged) > ledgerMaxEntries {
		merged = merged[:ledgerMaxEntries]
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, ledgerKey(project), string(b))
}

// DeleteLedgerEntry removes one entry by key from a project's ledger.
func (e *Engine) DeleteLedgerEntry(ctx context.Context, project, key string) error {
	if project == "" {
		return nil
	}
	want := normalizeLedgerKey(key)
	existing := e.Ledger(ctx, project)
	var kept []LedgerEntry
	for _, it := range existing {
		if normalizeLedgerKey(it.Key) != want {
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 {
		return e.ResetLedger(ctx, project)
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return e.store.SetKV(ctx, ledgerKey(project), string(b))
}

// ResetLedger clears a project's improvement ledger.
func (e *Engine) ResetLedger(ctx context.Context, project string) error {
	return e.store.WriteKV(ctx, nil, []string{ledgerKey(project)})
}

// saveLedgerEntries records any outputs.ledger_entries emitted by a step.
func (e *Engine) saveLedgerEntries(ctx context.Context, r *store.Run, v *store.Visit) {
	if r.Project == "" || len(v.Outputs) == 0 {
		return
	}
	var out struct {
		Entries []LedgerEntry `json:"ledger_entries"`
	}
	if json.Unmarshal(v.Outputs, &out) != nil || len(out.Entries) == 0 {
		return
	}
	for i := range out.Entries {
		if out.Entries[i].RunID == "" {
			out.Entries[i].RunID = r.ID
		}
		if out.Entries[i].PRURL == "" {
			out.Entries[i].PRURL = r.PRURL
		}
	}
	if err := e.UpsertLedger(ctx, r.Project, out.Entries...); err != nil {
		slog.Warn("save project ledger", "run", r.ID, "err", err)
		return
	}
	e.event(ctx, r, v.Node, "ledger", fmt.Sprintf("%s#%d updated the improvement ledger (%d entries)", v.Node, v.Visit, len(out.Entries)))
}

// ledgerSuppressed reports whether key is already shipped, rejected, or won't-fix
// so discovery/create_tasks will not re-queue it.
func ledgerSuppressed(entries []LedgerEntry, key string) (LedgerEntry, bool) {
	want := normalizeLedgerKey(key)
	for _, it := range entries {
		if normalizeLedgerKey(it.Key) == want {
			switch it.Status {
			case LedgerShipped, LedgerRejected, LedgerWontFix:
				return it, true
			}
		}
	}
	return LedgerEntry{}, false
}

// writeLedgerContext appends a compact view of the project's improvement ledger
// into a model node's context so recurring audits avoid duplicates and respect
// prior decisions.
func writeLedgerContext(sb *strings.Builder, entries []LedgerEntry) {
	if len(entries) == 0 {
		return
	}
	sb.WriteString("## Project improvement ledger\n\nEarlier improvements, shipped work, reverts, and dismissed findings for this project. Do not re-propose items already marked shipped, rejected, or wont_fix:\n\n")
	used := 0
	for _, it := range entries {
		when := ""
		if it.UpdatedAt > 0 {
			when = " (" + time.UnixMilli(it.UpdatedAt).UTC().Format("2006-01-02") + ")"
		}
		line := fmt.Sprintf("- [%s] `%s`: %s%s", it.Status, it.Key, oneLine(it.Title, 120), when)
		if it.Summary != "" {
			line += " — " + oneLine(it.Summary, 160)
		}
		line += "\n"
		if used+len(line) > ledgerContextLimit {
			sb.WriteString("- … (older ledger entries omitted)\n")
			break
		}
		sb.WriteString(line)
		used += len(line)
	}
	sb.WriteString("\n")
}

func normalizeLedgerKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-")
	}
	return out
}

func normalizeLedgerStatus(s, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LedgerProposed, LedgerShipped, LedgerReverted, LedgerRejected, LedgerWontFix:
		return strings.ToLower(strings.TrimSpace(s))
	}
	if fallback != "" {
		return fallback
	}
	return LedgerProposed
}
