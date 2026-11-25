package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
)

// AttentionReport gathers every item currently needing human judgment or action
// across active runs, failed runs, planning errors, improvement proposals, and
// product story maps.
type AttentionReport struct {
	Counts          AttentionCounts      `json:"counts"`
	Gates           []AttentionGate      `json:"gates"`
	FailedRuns      []AttentionFailedRun `json:"failed_runs"`
	Proposals       []AttentionProposal  `json:"proposals"`
	PlanIssues      []AttentionPlanIssue `json:"plan_issues"`
	ShippedUnmarked []AttentionShipped   `json:"shipped_unmarked"`
	OpenQuestions   []AttentionQuestion  `json:"open_questions"`
}

type AttentionCounts struct {
	Total           int `json:"total"`
	Gates           int `json:"gates"`
	FailedRuns      int `json:"failed_runs"`
	Proposals       int `json:"proposals"`
	PlanIssues      int `json:"plan_issues"`
	ShippedUnmarked int `json:"shipped_unmarked"`
	OpenQuestions   int `json:"open_questions"`
}

type AttentionProposal struct {
	ID           string   `json:"id"`
	TargetKind   string   `json:"target_kind"`
	Target       string   `json:"target"`
	Theme        string   `json:"theme,omitempty"`
	Title        string   `json:"title"`
	Reason       string   `json:"reason"`
	BeforeText   string   `json:"before_text"`
	AfterText    string   `json:"after_text"`
	EvidenceRuns []string `json:"evidence_runs"`
	CreatedAt    int64    `json:"created_at"`
}

type GateLoopInfo struct {
	Outcome   string `json:"outcome"`
	Target    string `json:"target"`
	Used      int    `json:"used"`
	MaxVisits int    `json:"max_visits"`
}

type GatePrevStep struct {
	Node    string         `json:"node"`
	Outcome string         `json:"outcome,omitempty"`
	Summary string         `json:"summary,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
}

type AttentionGate struct {
	RunID       string          `json:"run_id"`
	Project     string          `json:"project,omitempty"`
	FlowName    string          `json:"flow_name"`
	FlowVersion int             `json:"flow_version"`
	TaskID      string          `json:"task_id,omitempty"`
	TaskTitle   string          `json:"task_title,omitempty"`
	Seq         int             `json:"seq"`
	Node        string          `json:"node"`
	IsPause     bool            `json:"is_pause,omitempty"`
	NextNode    string          `json:"next_node,omitempty"`
	Prompt      string          `json:"prompt"`
	Outcomes    []string        `json:"outcomes"`
	Deadline    int64           `json:"deadline,omitempty"`
	StartedAt   int64           `json:"started_at"`
	PRURL       string          `json:"pr_url,omitempty"`
	Branch      string          `json:"branch,omitempty"`
	CostUSD     float64         `json:"cost_usd"`
	Diff        json.RawMessage `json:"diff,omitempty"`
	ExtraVisits int             `json:"extra_visits,omitempty"`
	Prev        *GatePrevStep   `json:"prev,omitempty"`
	Loops       []GateLoopInfo  `json:"loops,omitempty"`
}

type AttentionFailedRun struct {
	RunID       string  `json:"run_id"`
	Project     string  `json:"project,omitempty"`
	FlowName    string  `json:"flow_name"`
	FlowVersion int     `json:"flow_version"`
	TaskID      string  `json:"task_id,omitempty"`
	TaskTitle   string  `json:"task_title,omitempty"`
	LastNode    string  `json:"last_node,omitempty"`
	Error       string  `json:"error,omitempty"`
	CostUSD     float64 `json:"cost_usd"`
	FinishedAt  int64   `json:"finished_at,omitempty"`
}

type AttentionPlanIssue struct {
	TaskID   string `json:"task_id"`
	Project  string `json:"project"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	FlowName string `json:"flow_name,omitempty"`
	Error    string `json:"error,omitempty"`
}

type AttentionShipped struct {
	Project    string   `json:"project"`
	MapID      string   `json:"map_id"`
	MapTitle   string   `json:"map_title"`
	UserTaskID string   `json:"user_task_id"`
	Title      string   `json:"title"`
	Release    string   `json:"release,omitempty"`
	RunID      string   `json:"run_id"`
	PRURL      string   `json:"pr_url,omitempty"`
	Metrics    []string `json:"metrics,omitempty"`
	FinishedAt int64    `json:"finished_at,omitempty"`
}

type AttentionQuestion struct {
	Project   string   `json:"project"`
	MapID     string   `json:"map_id"`
	MapTitle  string   `json:"map_title"`
	Kind      string   `json:"kind"` // task | release
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Release   string   `json:"release,omitempty"`
	Questions []string `json:"questions"`
}

// Attention collects every decision, failure, unverified shipment, and open
// product question currently needing human input. Optionally filtered to one
// project when project != "".
func (a *App) Attention(ctx context.Context, project string) (*AttentionReport, error) {
	rep := &AttentionReport{
		Gates:           []AttentionGate{},
		FailedRuns:      []AttentionFailedRun{},
		Proposals:       []AttentionProposal{},
		PlanIssues:      []AttentionPlanIssue{},
		ShippedUnmarked: []AttentionShipped{},
		OpenQuestions:   []AttentionQuestion{},
	}

	tasks, err := a.Store.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	taskByID := make(map[string]*store.Task, len(tasks))
	for _, t := range tasks {
		state, _ := a.TaskState(t)
		taskByID[t.ID] = state
		if project != "" && state.Project != project {
			continue
		}
		if state.Status == store.TaskPlanFailed || state.Status == TaskPlanInterrupted {
			rep.PlanIssues = append(rep.PlanIssues, AttentionPlanIssue{
				TaskID: state.ID, Project: state.Project, Title: state.Title,
				Status: state.Status, FlowName: state.FlowName, Error: state.Error,
			})
		}
	}

	// Waiting gates: every run currently waiting on a human.
	waiting, err := a.Store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.RunWaiting}, Project: project, Limit: 100})
	if err != nil {
		return nil, err
	}
	for _, r := range waiting {
		g, err := a.buildAttentionGate(ctx, r, taskByID[r.TaskID])
		if err != nil || g == nil {
			continue
		}
		rep.Gates = append(rep.Gates, *g)
	}
	sort.Slice(rep.Gates, func(i, j int) bool { return rep.Gates[i].StartedAt < rep.Gates[j].StartedAt })

	// Recent failed runs: only surface a flow's latest run when it is failed
	// (so fixing and re-running clears the failure from the inbox).
	runs, err := a.Store.ListRuns(ctx, store.RunFilter{Project: project, Limit: 200})
	if err != nil {
		return nil, err
	}
	seenFlow := map[string]bool{}
	for _, r := range runs {
		key := r.FlowName
		if r.TaskID != "" {
			key = "task:" + r.TaskID
		}
		if seenFlow[key] {
			continue
		}
		seenFlow[key] = true
		if r.Status != store.RunFailed {
			continue
		}
		title := r.FlowName
		if t := taskByID[r.TaskID]; t != nil {
			title = t.Title
		}
		rep.FailedRuns = append(rep.FailedRuns, AttentionFailedRun{
			RunID: r.ID, Project: r.Project, FlowName: r.FlowName, FlowVersion: r.FlowVersion,
			TaskID: r.TaskID, TaskTitle: title, LastNode: r.CurrentNode,
			Error: r.Error, CostUSD: r.CostUSD, FinishedAt: r.FinishedAt,
		})
		if len(rep.FailedRuns) >= 25 {
			break
		}
	}

	// Story-map items from local workspaces already checked out on disk (no
	// remote GitHub calls on inbox load).
	if a.Workspaces != nil {
		cfg := a.Cfg.Current()
		projects := make([]string, 0, len(cfg.Projects))
		for name := range cfg.Projects {
			if project == "" || name == project {
				projects = append(projects, name)
			}
		}
		sort.Strings(projects)
		for _, proj := range projects {
			work := a.Workspaces.WorkDir(proj)
			if _, err := os.Stat(filepath.Join(work, filepath.FromSlash(storymap.Root))); err != nil {
				continue
			}
			maps, err := storymap.List(work)
			if err != nil {
				continue
			}
			for _, ms := range maps {
				l, err := storymap.Load(work, ms.ID)
				if err != nil {
					continue
				}
				for _, rel := range l.Map.Releases {
					if len(rel.OpenQuestions) > 0 && rel.Status != "released" {
						rep.OpenQuestions = append(rep.OpenQuestions, AttentionQuestion{
							Project: proj, MapID: l.ID, MapTitle: l.Map.Title,
							Kind: "release", ID: rel.ID, Title: rel.Title,
							Questions: rel.OpenQuestions,
						})
					}
				}
				works, _ := a.MapWorks(ctx, proj, l.ID)
				latestByTask := map[string]*MapWork{}
				for _, w := range works {
					for _, tid := range w.Scope.Tasks {
						if _, exists := latestByTask[tid]; !exists {
							latestByTask[tid] = w
						}
					}
				}
				for _, ut := range l.Tasks {
					if len(ut.OpenQuestions) > 0 && ut.Status != "done" {
						rep.OpenQuestions = append(rep.OpenQuestions, AttentionQuestion{
							Project: proj, MapID: l.ID, MapTitle: l.Map.Title,
							Kind: "task", ID: ut.ID, Title: ut.Title, Release: ut.Release,
							Questions: ut.OpenQuestions,
						})
					}
					if ut.Status != "done" {
						if w := latestByTask[ut.ID]; w != nil && w.Run != nil && w.Run.Status == store.RunSucceeded {
							rep.ShippedUnmarked = append(rep.ShippedUnmarked, AttentionShipped{
								Project: proj, MapID: l.ID, MapTitle: l.Map.Title,
								UserTaskID: ut.ID, Title: ut.Title, Release: ut.Release,
								RunID: w.Run.ID, PRURL: w.Run.PRURL, Metrics: ut.Metrics,
								FinishedAt: w.Run.FinishedAt,
							})
						}
					}
				}
			}
		}
	}

	// Pending improvement proposals (global or targeting this project).
	if props, err := a.Store.ListProposals(ctx, store.ProposalFilter{Status: store.ProposalPending, Limit: 25}); err == nil {
		for _, p := range props {
			if project != "" && (p.TargetKind == TargetProjectAgent || p.TargetKind == TargetProjectPlanner) && p.Target != project {
				continue
			}
			rep.Proposals = append(rep.Proposals, AttentionProposal{
				ID: p.ID, TargetKind: p.TargetKind, Target: p.Target, Theme: p.Theme,
				Title: p.Title, Reason: p.Reason, BeforeText: p.BeforeText, AfterText: p.AfterText,
				EvidenceRuns: p.EvidenceRuns, CreatedAt: p.CreatedAt,
			})
		}
	}

	rep.Counts = AttentionCounts{
		Gates:           len(rep.Gates),
		FailedRuns:      len(rep.FailedRuns),
		Proposals:       len(rep.Proposals),
		PlanIssues:      len(rep.PlanIssues),
		ShippedUnmarked: len(rep.ShippedUnmarked),
		OpenQuestions:   len(rep.OpenQuestions),
	}
	rep.Counts.Total = rep.Counts.Gates + rep.Counts.FailedRuns + rep.Counts.Proposals + rep.Counts.PlanIssues + rep.Counts.ShippedUnmarked + rep.Counts.OpenQuestions
	return rep, nil
}

func (a *App) buildAttentionGate(ctx context.Context, r *store.Run, task *store.Task) (*AttentionGate, error) {
	visits, err := a.Store.Visits(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	var waiting *store.Visit
	var prev *store.Visit
	for _, v := range visits {
		if v.Status == store.VisitWaiting {
			waiting = v
		} else if waiting == nil && v.Status == store.VisitSucceeded {
			prev = v
		}
	}
	if waiting == nil {
		return nil, nil
	}
	g := &AttentionGate{
		RunID: r.ID, Project: r.Project, FlowName: r.FlowName, FlowVersion: r.FlowVersion,
		TaskID: r.TaskID, Seq: waiting.Seq, Node: waiting.Node, Prompt: waiting.Prompt,
		Deadline: waiting.Deadline, StartedAt: waiting.StartedAt, PRURL: r.PRURL,
		Branch: r.Branch, CostUSD: r.CostUSD, Diff: r.Diff, ExtraVisits: r.ExtraVisits,
	}
	if task != nil {
		g.TaskTitle = task.Title
	}
	if prev != nil {
		var out map[string]any
		json.Unmarshal(prev.Outputs, &out)
		delete(out, "log_tail")
		g.Prev = &GatePrevStep{Node: prev.Node, Outcome: prev.Outcome, Summary: prev.Summary, Outputs: out}
	}
	if waiting.Node == flow.NodePause {
		g.IsPause = true
		g.Outcomes = []string{flow.OutcomeContinue, flow.OutcomeStop}
		var out map[string]string
		json.Unmarshal(waiting.Outputs, &out)
		g.NextNode = out["next_node"]
		return g, nil
	}
	if fv, err := a.Store.GetFlow(ctx, r.FlowName, r.FlowVersion); err == nil {
		if f, err := flow.Parse([]byte(fv.YAML)); err == nil {
			res := resolve.Resolve(f, a.Cfg.Current())
			if n := res.Nodes[waiting.Node]; n != nil {
				for _, o := range n.Outcomes {
					if o != flow.OutcomeTimeout {
						g.Outcomes = append(g.Outcomes, o)
					}
					target := n.Next[o]
					if target == "" || flow.Terminal(target) {
						continue
					}
					tn := res.Nodes[target]
					if tn == nil || tn.MaxVisits == 0 {
						continue
					}
					used := 0
					for _, pv := range visits {
						if pv.Node == target && pv.Seq > r.ResumeSeq {
							used++
						}
					}
					if used > 0 {
						g.Loops = append(g.Loops, GateLoopInfo{
							Outcome: o, Target: target, Used: used, MaxVisits: tn.MaxVisits + r.ExtraVisits,
						})
					}
				}
			}
		}
	}
	return g, nil
}

// RunExplanation is a compact, human- and LLM-friendly synthesis of how a run
// is being orchestrated: task intent, flow path taken, loops, active gate, and
// step outcomes.
type RunExplanation struct {
	Run          *store.Run      `json:"run"`
	TaskTitle    string          `json:"task_title,omitempty"`
	TaskBody     string          `json:"task_body,omitempty"`
	FlowSummary  string          `json:"flow_summary,omitempty"`
	ActiveGate   *AttentionGate  `json:"active_gate,omitempty"`
	Steps        []ExplainedStep `json:"steps"`
	RecentEvents []string        `json:"recent_events,omitempty"`
	Retro        *Retro          `json:"retro,omitempty"`
}

type ExplainedStep struct {
	Seq        int            `json:"seq"`
	Node       string         `json:"node"`
	Visit      int            `json:"visit"`
	Type       string         `json:"type"`
	Status     string         `json:"status"`
	Outcome    string         `json:"outcome,omitempty"`
	Model      string         `json:"model,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Error      string         `json:"error,omitempty"`
	DecidedBy  string         `json:"decided_by,omitempty"`
	Outputs    map[string]any `json:"outputs,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	CostUSD    float64        `json:"cost_usd,omitempty"`
	Lane       string         `json:"lane,omitempty"`
}

// ExplainRun builds a complete orchestration narrative for a run.
func (a *App) ExplainRun(ctx context.Context, runID string) (*RunExplanation, error) {
	r, err := a.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	exp := &RunExplanation{Run: r, Steps: []ExplainedStep{}}
	var task *store.Task
	if r.TaskID != "" {
		if t, err := a.Store.GetTask(ctx, r.TaskID); err == nil {
			task = t
			exp.TaskTitle = t.Title
			exp.TaskBody = clip(strings.TrimSpace(t.Body), 3000)
		}
	}
	if fv, err := a.Store.GetFlow(ctx, r.FlowName, r.FlowVersion); err == nil {
		if f, err := flow.Parse([]byte(fv.YAML)); err == nil {
			exp.FlowSummary = fmt.Sprintf("%s (start: %s, %d nodes)", f.Spec.Description, f.Spec.Start, len(f.Spec.Nodes))
		}
	}
	visits, err := a.Store.Visits(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range visits {
		var out map[string]any
		json.Unmarshal(v.Outputs, &out)
		if lt, ok := out["log_tail"].(string); ok && len(lt) > 800 {
			out["log_tail"] = clipEnd(lt, 800)
		}
		var dur int64
		if v.StartedAt > 0 {
			end := v.FinishedAt
			if end == 0 {
				end = store.Now()
			}
			dur = end - v.StartedAt
		}
		exp.Steps = append(exp.Steps, ExplainedStep{
			Seq: v.Seq, Node: v.Node, Visit: v.Visit, Type: v.Type, Status: v.Status,
			Outcome: v.Outcome, Model: v.Model, Summary: clip(v.Summary, 600),
			Error: clip(v.Error, 600), DecidedBy: v.DecidedBy, Outputs: out,
			DurationMs: dur, CostUSD: v.CostUSD, Lane: v.Lane,
		})
	}
	if r.Status == store.RunWaiting {
		exp.ActiveGate, _ = a.buildAttentionGate(ctx, r, task)
	}
	if evs, err := a.Store.Events(ctx, "run_id", r.ID, 25); err == nil {
		for _, e := range evs {
			exp.RecentEvents = append(exp.RecentEvents, fmt.Sprintf("[%s] %s %s", e.Kind, e.Node, clip(e.Message, 240)))
		}
	}
	if retro, err := a.GetRetro(ctx, r.ID); err == nil && retro != nil {
		exp.Retro = retro
	}
	return exp, nil
}

// VisitDetail returns a visit and a bounded transcript preview for inspection.
func (a *App) VisitDetail(ctx context.Context, runID string, seq int) (*store.Visit, error) {
	v, err := a.Store.GetVisit(ctx, runID, seq)
	if errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return v, err
}
