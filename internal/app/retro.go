package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
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
	started := *r // the goroutine below keeps writing to r
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
	return &started, nil
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
		if state := a.prOutcome(ctx, run.PRURL); state != "" {
			fmt.Fprintf(&b, " (PR state: %s)", state)
		}
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
	transcriptBudget := 6000
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
		if v.TranscriptKey != "" && a.Obj != nil && transcriptBudget > 0 {
			if data, err := a.Obj.Get(ctx, v.TranscriptKey); err == nil {
				if digest := transcriptDigest(data); digest != "" {
					digest = clip(digest, min(1000, transcriptBudget))
					transcriptBudget -= len(digest)
					fmt.Fprintf(&b, "transcript digest: %s\n", digest)
				}
			}
		}
		// Log tails are the most useful evidence for failures and loops.
		if v.LogTail != "" && (v.Status != store.VisitSucceeded || v.Visit > 1) && budget > 0 {
			tail := clipEnd(v.LogTail, min(3000, budget))
			budget -= len(tail)
			fmt.Fprintf(&b, "log tail:\n%s\n", tail)
		}
	}
	writeStepInstructions(&b, a, ctx, run, visits)
	events, _ := a.Store.Events(ctx, "run_id", run.ID, 80)
	if len(events) > 0 {
		b.WriteString("\n## Events\n")
		for _, e := range events {
			fmt.Fprintf(&b, "- %s %s %s\n", e.Kind, e.Node, clip(e.Message, 300))
		}
	}
	return b.String(), nil
}

// writeStepInstructions adds the instructions each visited step ran with,
// from the run's pinned snapshot: the flow YAML only names presets, and the
// catalog may have changed since the run.
func writeStepInstructions(b *strings.Builder, a *App, ctx context.Context, run *store.Run, visits []*store.Visit) {
	if a.Engine == nil {
		return
	}
	res, err := a.Engine.PinnedResolved(ctx, run)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	budget := 20000
	var parts []string
	for _, v := range visits {
		n := res.Nodes[v.Node]
		if n == nil || seen[v.Node] || strings.TrimSpace(n.Prompt) == "" || budget <= 0 {
			continue
		}
		seen[v.Node] = true
		kind := n.Preset
		if kind == "" {
			kind = string(n.Type)
		}
		text := clip(strings.TrimSpace(n.Prompt), min(2000, budget))
		budget -= len(text)
		parts = append(parts, fmt.Sprintf("### %s (%s)\n%s\n", v.Node, kind, text))
	}
	if len(parts) > 0 {
		b.WriteString("\n## Instructions the steps ran with (pinned when the run started)\n")
		b.WriteString(strings.Join(parts, "\n"))
	}
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

func parsePRURL(raw string) (github.Repo, int, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return github.Repo{}, 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return github.Repo{}, 0, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return github.Repo{}, 0, false
	}
	return github.Repo{Host: u.Host, Owner: parts[0], Name: parts[1]}, n, true
}

func (a *App) prOutcome(ctx context.Context, prURL string) string {
	if a.GitHub == nil || prURL == "" {
		return ""
	}
	repo, num, ok := parsePRURL(prURL)
	if !ok {
		return ""
	}
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pr, err := a.GitHub.PR(tctx, repo, num)
	if err != nil || pr == nil {
		return ""
	}
	if pr.Merged {
		return "merged"
	}
	if pr.State == "open" && pr.MergeableState != "" && pr.MergeableState != "clean" && pr.MergeableState != "unknown" {
		return "open (" + pr.MergeableState + ")"
	}
	return pr.State
}

// transcriptDigest extracts a compact summary of tool usage, repeated commands,
// and prompt cache hit rate from an agent JSONL transcript so the grader and
// retro can detect in-step thrashing without loading megabytes of raw logs.
func transcriptDigest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	toolCounts := map[string]int{}
	cmdCounts := map[string]int{}
	var totalIn, totalCached int64
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	recordTool := func(name string, args map[string]any) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "mcp__flow__"), "flow_")
		toolCounts[name]++
		if cmd, ok := args["command"].(string); ok {
			c := oneLine(cmd)
			if len(c) > 120 {
				c = c[:120]
			}
			if c != "" {
				cmdCounts[c]++
			}
		}
	}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch t, _ := ev["type"].(string); t {
		case "tool_execution_start": // pi
			name, _ := ev["toolName"].(string)
			args, _ := ev["args"].(map[string]any)
			recordTool(name, args)
		case "tool_use": // opencode
			if part, ok := ev["part"].(map[string]any); ok {
				name, _ := part["tool"].(string)
				state, _ := part["state"].(map[string]any)
				args, _ := state["input"].(map[string]any)
				recordTool(name, args)
			}
		case "item.started": // codex
			if item, ok := ev["item"].(map[string]any); ok {
				switch item["type"] {
				case "command_execution":
					cmd, _ := item["command"].(string)
					recordTool("bash", map[string]any{"command": cmd})
				case "mcp_tool_call":
					name, _ := item["tool"].(string)
					args, _ := item["arguments"].(map[string]any)
					recordTool(name, args)
				case "file_change":
					recordTool("edit", nil)
				}
			}
		case "message_end": // pi usage
			if msg, ok := ev["message"].(map[string]any); ok {
				if usage, ok := msg["usage"].(map[string]any); ok {
					if v, ok := usage["input"].(float64); ok {
						totalIn += int64(v)
					}
					if v, ok := usage["cacheRead"].(float64); ok {
						totalCached += int64(v)
					}
				}
			}
		}
	}
	if len(toolCounts) == 0 && totalIn == 0 {
		return ""
	}
	var parts []string
	if len(toolCounts) > 0 {
		names := make([]string, 0, len(toolCounts))
		for k := range toolCounts {
			names = append(names, k)
		}
		sort.Slice(names, func(i, j int) bool {
			if toolCounts[names[i]] != toolCounts[names[j]] {
				return toolCounts[names[i]] > toolCounts[names[j]]
			}
			return names[i] < names[j]
		})
		var tc []string
		for _, k := range names {
			tc = append(tc, fmt.Sprintf("%s=%d", k, toolCounts[k]))
		}
		parts = append(parts, "tools("+strings.Join(tc, ", ")+")")
	}
	var repeated []string
	for cmd, n := range cmdCounts {
		if n >= 3 {
			repeated = append(repeated, fmt.Sprintf("%dx %q", n, cmd))
		}
	}
	sort.Strings(repeated)
	if len(repeated) > 0 {
		if len(repeated) > 4 {
			repeated = repeated[:4]
		}
		parts = append(parts, "REPEATED COMMANDS: "+strings.Join(repeated, "; "))
	}
	if totalIn+totalCached > 0 && totalCached > 0 {
		denom := totalIn
		if totalCached > denom {
			denom = totalIn + totalCached
		}
		parts = append(parts, fmt.Sprintf("cache_hit=%d%%", (100*totalCached)/denom))
	}
	return strings.Join(parts, "; ")
}
