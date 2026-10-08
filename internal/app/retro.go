package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/store"
)

// Retro is an LLM's review of one run: what went well, what went wrong, and
// what to change in the flow, the catalog or ai-flow's primitives.
type Retro struct {
	Status     string       `json:"status"` // pending | done | failed
	Model      string       `json:"model,omitempty"`
	StartedAt  int64        `json:"started_at"`
	FinishedAt int64        `json:"finished_at,omitempty"`
	Error      string       `json:"error,omitempty"`
	Report     *RetroReport `json:"report,omitempty"`
	Note       string       `json:"note,omitempty"` // what the user asked to focus on
}

type RetroReport struct {
	Summary     string            `json:"summary"`
	WentWell    []string          `json:"went_well"`
	Problems    []RetroProblem    `json:"problems"`
	Suggestions []RetroSuggestion `json:"suggestions"`
}

type RetroProblem struct {
	Node     string `json:"node,omitempty"`
	Evidence string `json:"evidence"`
	Impact   string `json:"impact,omitempty"`
}

// RetroSuggestion kinds: flow (this flow), preset (a catalog node), primitive
// (a new or changed engine capability: node type, action, control), prompt,
// model, config (project or catalog settings).
type RetroSuggestion struct {
	Kind     string `json:"kind"`
	Target   string `json:"target,omitempty"`
	Change   string `json:"change"`
	Why      string `json:"why"`
	Priority string `json:"priority,omitempty"` // high | medium | low
}

func kvRetro(runID string) string { return "retro/" + runID }

// GetRetro returns a run's retrospective, or nil if none was requested.
func (a *App) GetRetro(ctx context.Context, runID string) (*Retro, error) {
	raw, err := a.Store.GetKV(ctx, kvRetro(runID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Retro
	return &r, json.Unmarshal([]byte(raw), &r)
}

func (a *App) saveRetro(ctx context.Context, runID string, r *Retro) {
	b, _ := json.Marshal(r)
	if err := a.Store.SetKV(ctx, kvRetro(runID), string(b)); err != nil {
		slog.Error("saving retrospective", "run", runID, "err", err)
	}
	a.Hub.Publish(hub.Event{Type: "run", ID: runID})
}

// StartRetro reviews a run in the background (replacing an earlier review).
func (a *App) StartRetro(ctx context.Context, runID, note string) (*Retro, error) {
	run, err := a.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if prev, _ := a.GetRetro(ctx, runID); prev != nil && prev.Status == "pending" && store.Now()-prev.StartedAt < (20*time.Minute).Milliseconds() {
		return prev, nil
	}
	model := a.Cfg.Current().Catalog.Planner.Model
	if model == "" {
		return nil, fmt.Errorf("no planner model is configured")
	}
	r := &Retro{Status: "pending", Model: model, StartedAt: store.Now(), Note: strings.TrimSpace(note)}
	a.saveRetro(ctx, runID, r)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		report, err := a.retro(ctx, run, model, r.Note)
		r.FinishedAt = store.Now()
		if err != nil {
			r.Status, r.Error = "failed", err.Error()
		} else {
			r.Status, r.Report = "done", report
		}
		a.saveRetro(ctx, runID, r)
	}()
	return r, nil
}

const retroSystem = `You review one finished (or stuck) run of ai-flow, a system that runs software tasks as state machines ("flows") of small steps. Your job is to tell the owner where the run wasted time, money or attempts, where it went wrong, and what to change so the next run goes better. Be concrete and cite evidence from the run (node ids, outcomes, errors, durations, costs, outputs). Do not invent facts that are not in the run record; say so when the record is not enough to tell.

Suggestions can target:
- flow: this flow's structure (nodes, edges, loops, max_visits, gates, checks)
- preset: a reusable catalog node (its prompt, outcomes, outputs, defaults)
- primitive: something ai-flow itself lacks or should change (a node type, an action, a control like loops/limits/parallelism, data passed between nodes)
- prompt: a node's instructions
- model: which model a node uses
- config: project or catalog settings (budgets, timeouts, grants, deploy checks)
Prefer few, high-value suggestions over many small ones. Mark each high, medium or low priority.

ai-flow building blocks today:
%s
Reply with one JSON object and nothing else:
{"summary": "<2-4 sentences>", "went_well": ["..."], "problems": [{"node": "<id or empty>", "evidence": "...", "impact": "..."}], "suggestions": [{"kind": "flow|preset|primitive|prompt|model|config", "target": "<node, preset or setting>", "change": "...", "why": "...", "priority": "high|medium|low"}]}`

func (a *App) retro(ctx context.Context, run *store.Run, model, note string) (*RetroReport, error) {
	cfg := a.Cfg.Current()
	record, err := a.runRecord(ctx, run)
	if err != nil {
		return nil, err
	}
	user := record
	if note != "" {
		user += "\n\nThe owner asks you to focus on: " + note
	}
	msgs := []llm.Message{
		{Role: "system", Content: fmt.Sprintf(retroSystem, buildingBlocks(cfg))},
		{Role: "user", Content: user},
	}
	for attempt := 0; attempt < 2; attempt++ {
		reply, _, err := a.LLM.Chat(ctx, model, msgs, llm.Options{Temperature: 0.2, Stream: cfg.Catalog.Planner.Stream})
		if err != nil {
			return nil, err
		}
		s := strings.TrimSpace(reply)
		if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
			s = s[i : j+1]
		}
		var rep RetroReport
		if err := json.Unmarshal([]byte(s), &rep); err == nil && rep.Summary != "" {
			return &rep, nil
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
			llm.Message{Role: "user", Content: "Reply again with only the JSON object described."})
	}
	return nil, fmt.Errorf("the model did not return a usable review")
}

// buildingBlocks summarizes node types, actions and presets for the reviewer.
func buildingBlocks(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("Node types: llm (one structured call), agent (coding agent with tools), check (shell command; exit code or JSON outputs pick the outcome), gate (human decision with a note), switch (CEL routing), action (built-in), parallel + join (read-only branches at once). Loops are back edges bounded by max_visits/on_exhausted; on_limit handles rate limits, quota, context overflow and budget; runs can be resumed from a node.\n")
	b.WriteString("Actions:\n")
	for _, name := range flow.ActionNames() {
		fmt.Fprintf(&b, "- %s: %s\n", name, flow.Actions[name].Doc)
	}
	b.WriteString("Catalog presets:\n")
	names := make([]string, 0, len(cfg.Catalog.Presets))
	for n := range cfg.Catalog.Presets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Catalog.Presets[n]
		fmt.Fprintf(&b, "- %s (%s): %s\n", n, p.Type, oneLine(firstNonEmpty(p.WhenToUse, p.Description)))
	}
	return b.String()
}

// runRecord renders everything known about a run for the reviewer.
func (a *App) runRecord(ctx context.Context, run *store.Run) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s of flow %s v%d (project %s): status %s", run.ID, run.FlowName, run.FlowVersion, orNone(run.Project), run.Status)
	if run.Error != "" {
		fmt.Fprintf(&b, ", error: %s", clip(run.Error, 800))
	}
	fmt.Fprintf(&b, "\nCost $%.2f, %d tokens", run.CostUSD, run.Tokens)
	if run.StartedAt > 0 {
		end := run.FinishedAt
		if end == 0 {
			end = store.Now()
		}
		fmt.Fprintf(&b, ", wall time %s", time.Duration(end-run.StartedAt)*time.Millisecond)
	}
	if run.PRURL != "" {
		fmt.Fprintf(&b, ", PR %s", run.PRURL)
	}
	if run.Resumes > 0 {
		fmt.Fprintf(&b, ", resumed %d times (latest note: %s)", run.Resumes, run.ResumeNote)
	}
	b.WriteString("\n")
	if run.TaskID != "" {
		if t, err := a.Store.GetTask(ctx, run.TaskID); err == nil {
			fmt.Fprintf(&b, "\n## Task\n%s\n%s\n", t.Title, clip(t.Body, 4000))
		}
	}
	if fv, err := a.Store.GetFlow(ctx, run.FlowName, run.FlowVersion); err == nil {
		fmt.Fprintf(&b, "\n## Flow YAML\n%s\n", clip(fv.YAML, 16000))
	}
	visits, err := a.Store.Visits(ctx, run.ID)
	if err != nil {
		return "", err
	}
	b.WriteString("\n## Steps, in order\n")
	budget := 60000
	for _, v := range visits {
		fmt.Fprintf(&b, "\n### #%d %s (%s, visit %d): %s", v.Seq, v.Node, v.Type, v.Visit, v.Status)
		if v.Outcome != "" {
			fmt.Fprintf(&b, " → %s", v.Outcome)
		}
		b.WriteString("\n")
		if v.StartedAt > 0 && v.FinishedAt > 0 {
			fmt.Fprintf(&b, "duration %s; ", time.Duration(v.FinishedAt-v.StartedAt)*time.Millisecond)
		}
		if v.Model != "" {
			fmt.Fprintf(&b, "model %s, %d calls, %d in / %d out tokens, $%.3f; ", v.Model, v.LLMCalls, v.TokensIn, v.TokensOut, v.CostUSD)
		}
		if v.DecidedBy != "" {
			fmt.Fprintf(&b, "decided by %s; ", v.DecidedBy)
		}
		b.WriteString("\n")
		if v.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", clip(v.Error, 1500))
		}
		if v.Summary != "" {
			fmt.Fprintf(&b, "summary: %s\n", clip(v.Summary, 1500))
		}
		if o := string(v.Outputs); o != "" && o != "null" && o != "{}" {
			fmt.Fprintf(&b, "outputs: %s\n", clip(o, 2000))
		}
		// Log tails are the most useful evidence for failures and loops.
		if v.LogTail != "" && (v.Status != store.VisitSucceeded || v.Visit > 1) && budget > 0 {
			tail := clipEnd(v.LogTail, min(3000, budget))
			budget -= len(tail)
			fmt.Fprintf(&b, "log tail:\n%s\n", tail)
		}
	}
	events, _ := a.Store.Events(ctx, "run_id", run.ID, 80)
	if len(events) > 0 {
		b.WriteString("\n## Events\n")
		for _, e := range events {
			fmt.Fprintf(&b, "- %s %s %s\n", e.Kind, e.Node, clip(e.Message, 300))
		}
	}
	return b.String(), nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}

func clipEnd(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "(truncated) …" + s[len(s)-n:]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
