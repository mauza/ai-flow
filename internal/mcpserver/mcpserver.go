// Package mcpserver exposes ai-flow's orchestration state, product health, and
// (optionally) human steering actions as a Model Context Protocol (MCP) server.
//
// It runs both in-process over Streamable HTTP (/mcp on the control plane) and
// as a stdio CLI bridge (`ai-flow mcp -url http://ai-flow:8080`) for local LLM
// clients. Read-only inspection tools are always registered; steering tools
// require explicit opt-in via Options.AllowSteer.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

// Options controls what capabilities the MCP server exposes.
type Options struct {
	// AllowSteer enables write/steering tools (decide_gate, steer_run,
	// resume_run, create_task, send_story_to_flow, start_retro).
	AllowSteer bool
	// Actor labels human/LLM-initiated decisions (default "mcp").
	Actor string
}

// Backend abstracts the control plane so the same MCP tool definitions work
// in-process on the server and over HTTP in `ai-flow mcp`.
type Backend interface {
	Overview(ctx context.Context) (any, error)
	Attention(ctx context.Context, project string) (*app.AttentionReport, error)
	ListRuns(ctx context.Context, project, flowName, status string, limit int) ([]*store.Run, error)
	ExplainRun(ctx context.Context, runID string) (*app.RunExplanation, error)
	InspectVisit(ctx context.Context, runID string, seq int) (map[string]any, error)
	ListFlows(ctx context.Context) ([]*store.FlowVersion, error)
	GetFlow(ctx context.Context, name string, version int) (map[string]any, error)
	ProjectSummary(ctx context.Context, project string) (*app.ProjectSummary, error)
	ProjectDigest(ctx context.Context, project string) (string, error)
	GetRetro(ctx context.Context, runID string) (*app.Retro, error)
	ImprovementSummary(ctx context.Context, project string) (*app.ImprovementSummary, error)
	ListRunGrades(ctx context.Context, runID, project, rubric, blame string, limit int) ([]*store.RunGrade, error)
	ListProposals(ctx context.Context, status string, limit int) ([]*store.ImprovementProposal, error)
	ListSchedules(ctx context.Context, project string) ([]app.ScheduleView, error)
	ProjectLedger(ctx context.Context, project string) ([]engine.LedgerEntry, error)
	ProjectBaselines(ctx context.Context, project string) (map[string]engine.BaselineEntry, error)
	ExportCatalogImprovements(ctx context.Context) (string, error)

	// Steering actions (only wired when Options.AllowSteer is true).
	DecideGate(ctx context.Context, runID string, seq int, outcome, note, by string, extendVisits bool) error
	SteerRun(ctx context.Context, runID string, pause *bool, note *string, by string) (*store.Run, error)
	ResumeRun(ctx context.Context, runID, node, note, by string, flowVersion int, refresh bool) (*store.Run, error)
	CreateTask(ctx context.Context, project, title, body string, plan bool) (*store.Task, error)
	SendStoryToFlow(ctx context.Context, project, mapID, kind, id, release string, plan bool) (*store.Task, error)
	StartRetro(ctx context.Context, runID, focus string) (*app.Retro, error)
	GradeRun(ctx context.Context, runID string) (map[string]any, error)
	RunImprover(ctx context.Context, project string, minFailures int) (*app.ImproverRunStatus, error)
	DisagreeGrade(ctx context.Context, gradeID string, disagreed bool, by, note string) (*store.RunGrade, error)
	ConfirmGrade(ctx context.Context, gradeID, by, note string) (*store.RunGrade, error)
	ApplyProposal(ctx context.Context, proposalID, by, note, afterText string, force bool) (*store.ImprovementProposal, []string, error)
	RejectProposal(ctx context.Context, proposalID, by, note string) (*store.ImprovementProposal, error)
	TriggerSchedule(ctx context.Context, project, name string, force bool) (*app.ScheduleState, error)
	PauseSchedule(ctx context.Context, project, name string, paused bool) error
	UpsertProjectLedger(ctx context.Context, project string, entry engine.LedgerEntry) ([]engine.LedgerEntry, error)
}

// NewServer creates an MCP server backed by b.
func NewServer(b Backend, opts Options) *mcp.Server {
	actor := opts.Actor
	if actor == "" {
		actor = "mcp"
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "ai-flow", Version: "v1"}, nil)
	registerReadTools(s, b)
	if opts.AllowSteer {
		registerSteerTools(s, b, actor)
	}
	return s
}

// Handler serves the MCP server over Streamable HTTP.
func Handler(b Backend, opts Options) http.Handler {
	srv := NewServer(b, opts)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

func jsonText(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(err.Error()), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil
}

func plainText(s string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}, nil
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func registerReadTools(s *mcp.Server, b Backend) {
	type emptyIn struct{}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_overview",
		Description: "Get ai-flow's current overview: configured projects, catalog models/presets/runtimes, active/waiting/queued run counts, and step duration/failure stats.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		v, err := b.Overview(ctx)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type attentionIn struct {
		Project string `json:"project,omitempty" jsonschema:"optional project name to filter by"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_attention",
		Description: "List everything in ai-flow currently needing human input: waiting gates (with questions, previous step findings, and loop counts), failed runs, planning issues, shipped story-map tasks awaiting verification, and open product questions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attentionIn) (*mcp.CallToolResult, any, error) {
		v, err := b.Attention(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type listRunsIn struct {
		Project string `json:"project,omitempty" jsonschema:"optional project name filter"`
		Flow    string `json:"flow,omitempty" jsonschema:"optional flow name filter"`
		Status  string `json:"status,omitempty" jsonschema:"optional status filter: queued, running, waiting, succeeded, failed, canceled"`
		Limit   int    `json:"limit,omitempty" jsonschema:"maximum runs to return (default 25, max 100)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_runs",
		Description: "List recent flow runs with their status, current step, branch, PR URL, cost, and errors.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listRunsIn) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 25
		}
		if limit > 100 {
			limit = 100
		}
		v, err := b.ListRuns(ctx, in.Project, in.Flow, in.Status, limit)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type runIDIn struct {
		RunID string `json:"run_id" jsonschema:"the run id (e.g. r-abc123)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "explain_run",
		Description: "Explain how ai-flow is orchestrating a specific run: task intent, flow summary, step-by-step visit timeline, loops, active gate question and context, costs, recent events, and any retrospective.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runIDIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.RunID) == "" {
			return errResult("run_id is required"), nil, nil
		}
		v, err := b.ExplainRun(ctx, in.RunID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type inspectVisitIn struct {
		RunID string `json:"run_id" jsonschema:"the run id"`
		Seq   int    `json:"seq" jsonschema:"the step sequence number (1-based)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "inspect_visit",
		Description: "Inspect one step (visit) of a run in detail: rendered prompt, structured outputs, error, log tail, token usage, and transcript tail if available.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in inspectVisitIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.RunID) == "" || in.Seq <= 0 {
			return errResult("run_id and positive seq are required"), nil, nil
		}
		v, err := b.InspectVisit(ctx, in.RunID, in.Seq)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_flows",
		Description: "List saved flows with their latest version, project, and linked task.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		v, err := b.ListFlows(ctx)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type getFlowIn struct {
		Name    string `json:"name" jsonschema:"the flow name"`
		Version int    `json:"version,omitempty" jsonschema:"optional flow version (0 or omitted for latest)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_flow",
		Description: "Read a flow's YAML state machine definition, validation issues, graph nodes/edges, and linked task.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getFlowIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Name) == "" {
			return errResult("name is required"), nil, nil
		}
		v, err := b.GetFlow(ctx, in.Name, in.Version)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type projectIn struct {
		Project string `json:"project" jsonschema:"the project name"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_product_summary",
		Description: "Get a decision-oriented summary of a project: delivery metrics and spend, story maps and release progress, live product metrics vs targets, open product questions, high-uncertainty bets, post-ship outcomes, and recent retro takeaways.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" {
			return errResult("project is required"), nil, nil
		}
		v, err := b.ProjectSummary(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_project_digest",
		Description: "Generate a clean, shareable Markdown status and value digest for a project so stakeholders can see how it is going and what is valuable next.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" {
			return errResult("project is required"), nil, nil
		}
		md, err := b.ProjectDigest(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := plainText(md)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_run_retro",
		Description: "Read the stored LLM retrospective review for a run (what went well, problems, and prioritized improvement suggestions).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runIDIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.RunID) == "" {
			return errResult("run_id is required"), nil, nil
		}
		v, err := b.GetRetro(ctx, in.RunID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		if v == nil {
			res, err := plainText("No retrospective has been generated for this run yet.")
			return res, nil, err
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_improvement_summary",
		Description: "Get the improvement loop summary: outcome and rubric pass rates, agent vs environment failure clusters, prompt version score trends, grader spend, and pending proposal counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attentionIn) (*mcp.CallToolResult, any, error) {
		v, err := b.ImprovementSummary(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type listGradesIn struct {
		RunID   string `json:"run_id,omitempty" jsonschema:"optional run id filter"`
		Project string `json:"project,omitempty" jsonschema:"optional project filter"`
		Rubric  string `json:"rubric,omitempty" jsonschema:"optional rubric filter (e.g. efficiency, instruction_adherence, unmentioned_workarounds)"`
		Blame   string `json:"blame,omitempty" jsonschema:"optional blame filter: agent or environment"`
		Limit   int    `json:"limit,omitempty" jsonschema:"max grades to return (default 50, max 200)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_run_grades",
		Description: "List stored automatic rubric grades for finished runs, including pass/fail, agent vs environment blame, failure theme, prompt versions, and human disagreement.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listGradesIn) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		v, err := b.ListRunGrades(ctx, in.RunID, in.Project, in.Rubric, in.Blame, limit)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type listProposalsIn struct {
		Status string `json:"status,omitempty" jsonschema:"optional status filter: pending, applied, or rejected"`
		Limit  int    `json:"limit,omitempty" jsonschema:"max proposals to return (default 50)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_improvement_proposals",
		Description: "List prompt/instruction edit proposals drafted by the improver, with before/after text, cited failing runs, and decision status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProposalsIn) (*mcp.CallToolResult, any, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		v, err := b.ListProposals(ctx, in.Status, limit)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_schedules",
		Description: "List configured recurring schedules across projects with their last run time, status, and backlog guard limits.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attentionIn) (*mcp.CallToolResult, any, error) {
		v, err := b.ListSchedules(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_project_ledger",
		Description: "Read a project's persistent improvement ledger (shipped, open, rejected, wont_fix items) used to avoid duplicate continuous-improvement proposals.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" {
			return errResult("project is required"), nil, nil
		}
		v, err := b.ProjectLedger(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_project_baselines",
		Description: "Read a project's stored metric ratchet baselines enforced on check nodes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" {
			return errResult("project is required"), nil, nil
		}
		v, err := b.ProjectBaselines(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "export_catalog_improvements",
		Description: "Export DB-applied improvement proposals (presets, skills, harnesses, project instructions) as YAML for GitOps sync back into git.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, any, error) {
		doc, err := b.ExportCatalogImprovements(ctx)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := plainText(doc)
		return res, nil, err
	})
}

func registerSteerTools(s *mcp.Server, b Backend, actor string) {
	type decideIn struct {
		RunID        string `json:"run_id" jsonschema:"the waiting run id"`
		Seq          int    `json:"seq" jsonschema:"the waiting gate visit sequence number"`
		Outcome      string `json:"outcome" jsonschema:"one of the gate's valid outcomes (e.g. approve, revise, stop, continue)"`
		Note         string `json:"note,omitempty" jsonschema:"optional direction passed to the next step as outputs.note"`
		ExtendVisits bool   `json:"extend_visits,omitempty" jsonschema:"set true to grant +1 loop round when requesting a revision"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "decide_gate",
		Description: "Submit a human decision for a waiting gate (or pause gate), optionally passing a note to the next step and granting +1 loop round.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in decideIn) (*mcp.CallToolResult, any, error) {
		if in.RunID == "" || in.Seq <= 0 || in.Outcome == "" {
			return errResult("run_id, seq, and outcome are required"), nil, nil
		}
		if err := b.DecideGate(ctx, in.RunID, in.Seq, in.Outcome, in.Note, actor, in.ExtendVisits); err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := plainText(fmt.Sprintf("Recorded gate decision %q on %s step %d.", in.Outcome, in.RunID, in.Seq))
		return res, nil, err
	})

	type steerIn struct {
		RunID string  `json:"run_id" jsonschema:"the active run id"`
		Pause *bool   `json:"pause,omitempty" jsonschema:"true to pause cleanly after the current step finishes, false to clear pause"`
		Note  *string `json:"note,omitempty" jsonschema:"direction injected into the context of subsequent steps"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "steer_run",
		Description: "Steer an active run without canceling it: request a pause after the current step finishes, and/or send a steering note for subsequent steps.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in steerIn) (*mcp.CallToolResult, any, error) {
		if in.RunID == "" {
			return errResult("run_id is required"), nil, nil
		}
		r, err := b.SteerRun(ctx, in.RunID, in.Pause, in.Note, actor)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(r)
		return res, nil, err
	})

	type resumeIn struct {
		RunID       string `json:"run_id" jsonschema:"the failed or canceled run id"`
		Node        string `json:"node,omitempty" jsonschema:"step to resume at (omit for the last visited step)"`
		Note        string `json:"note,omitempty" jsonschema:"direction for the resumed steps"`
		FlowVersion int    `json:"flow_version,omitempty" jsonschema:"optional newer flow version to resume on"`
		Refresh     bool   `json:"refresh,omitempty" jsonschema:"re-pin to current catalog runtimes and presets"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "resume_run",
		Description: "Resume a failed or canceled run on its existing branch from a chosen step with an optional note.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resumeIn) (*mcp.CallToolResult, any, error) {
		if in.RunID == "" {
			return errResult("run_id is required"), nil, nil
		}
		r, err := b.ResumeRun(ctx, in.RunID, in.Node, in.Note, actor, in.FlowVersion, in.Refresh)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(r)
		return res, nil, err
	})

	type createTaskIn struct {
		Project string `json:"project" jsonschema:"target project name"`
		Title   string `json:"title" jsonschema:"concise task title"`
		Body    string `json:"body,omitempty" jsonschema:"task details, context, and acceptance criteria"`
		Plan    *bool  `json:"plan,omitempty" jsonschema:"whether to plan a flow immediately (default true)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "create_task",
		Description: "Create a new task in a project and optionally start planning a flow for it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createTaskIn) (*mcp.CallToolResult, any, error) {
		plan := in.Plan == nil || *in.Plan
		t, err := b.CreateTask(ctx, in.Project, in.Title, in.Body, plan)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(t)
		return res, nil, err
	})

	type sendStoryIn struct {
		Project string `json:"project" jsonschema:"project name"`
		MapID   string `json:"map" jsonschema:"story map id"`
		Kind    string `json:"kind" jsonschema:"scope kind: task, activity, or phase"`
		ID      string `json:"id" jsonschema:"id of the user task, activity, or phase"`
		Release string `json:"release,omitempty" jsonschema:"optional release id filter for activity/phase"`
		Plan    *bool  `json:"plan,omitempty" jsonschema:"whether to plan a flow immediately (default true)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_story_to_flow",
		Description: "Send a user task, activity, or phase from a project's user story map to a new ai-flow task and plan a flow.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendStoryIn) (*mcp.CallToolResult, any, error) {
		plan := in.Plan == nil || *in.Plan
		t, err := b.SendStoryToFlow(ctx, in.Project, in.MapID, in.Kind, in.ID, in.Release, plan)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(t)
		return res, nil, err
	})

	type startRetroIn struct {
		RunID string `json:"run_id" jsonschema:"the run id to review"`
		Focus string `json:"focus,omitempty" jsonschema:"optional question or aspect for the reviewer to focus on"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "start_retro",
		Description: "Start an LLM retrospective review of a run to surface problems and improvement suggestions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in startRetroIn) (*mcp.CallToolResult, any, error) {
		if in.RunID == "" {
			return errResult("run_id is required"), nil, nil
		}
		r, err := b.StartRetro(ctx, in.RunID, in.Focus)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(r)
		return res, nil, err
	})

	type gradeRunIn struct {
		RunID string `json:"run_id" jsonschema:"the finished run id to grade against active rubrics"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "grade_run",
		Description: "Trigger automatic rubric grading for a run (records run facts, prompt versions, and rubric verdicts with agent vs environment blame).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in gradeRunIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.RunID) == "" {
			return errResult("run_id is required"), nil, nil
		}
		v, err := b.GradeRun(ctx, in.RunID)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type runImproverIn struct {
		Project     string `json:"project,omitempty" jsonschema:"optional project filter"`
		MinFailures int    `json:"min_failures,omitempty" jsonschema:"optional override for minimum agent-blamed failures per cluster"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "run_improver",
		Description: "Inspect unreviewed agent-blamed failure clusters and draft pending prompt edit proposals for human review.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runImproverIn) (*mcp.CallToolResult, any, error) {
		v, err := b.RunImprover(ctx, in.Project, in.MinFailures)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type disagreeGradeIn struct {
		GradeID   string `json:"grade_id" jsonschema:"the grade id (e.g. g-...)"`
		Disagreed bool   `json:"disagreed" jsonschema:"true to mark disagreement (excludes from improver), false to clear"`
		Note      string `json:"note,omitempty" jsonschema:"optional reason for disagreeing with the grade"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "disagree_grade",
		Description: "Record or clear human disagreement on a rubric grade so bad grades do not drive prompt edits.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in disagreeGradeIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.GradeID) == "" {
			return errResult("grade_id is required"), nil, nil
		}
		v, err := b.DisagreeGrade(ctx, in.GradeID, in.Disagreed, actor, in.Note)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type decideProposalIn struct {
		ProposalID string `json:"proposal_id" jsonschema:"the proposal id (e.g. imp-...)"`
		Decision   string `json:"decision" jsonschema:"apply or reject"`
		Note       string `json:"note,omitempty" jsonschema:"optional note (remembered on rejection)"`
		AfterText  string `json:"after_text,omitempty" jsonschema:"optional edited prompt text when applying"`
		Force      bool   `json:"force,omitempty" jsonschema:"force apply even if active runs would drift or target changed"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "decide_proposal",
		Description: "Apply or reject a pending improvement proposal. Rejected proposals are remembered so the improver avoids repeating them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in decideProposalIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.ProposalID) == "" {
			return errResult("proposal_id is required"), nil, nil
		}
		switch strings.ToLower(strings.TrimSpace(in.Decision)) {
		case "apply":
			p, warnings, err := b.ApplyProposal(ctx, in.ProposalID, actor, in.Note, in.AfterText, in.Force)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			res, err := jsonText(map[string]any{"proposal": p, "warnings": warnings})
			return res, nil, err
		case "reject":
			p, err := b.RejectProposal(ctx, in.ProposalID, actor, in.Note)
			if err != nil {
				return errResult(err.Error()), nil, nil
			}
			res, err := jsonText(map[string]any{"proposal": p})
			return res, nil, err
		default:
			return errResult("decision must be apply or reject"), nil, nil
		}
	})

	type confirmGradeIn struct {
		GradeID string `json:"grade_id" jsonschema:"the grade id (e.g. g-...)"`
		Note    string `json:"note,omitempty" jsonschema:"optional confirmation note"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "confirm_grade",
		Description: "Confirm agreement with a rubric grade to build Cohen's kappa calibration data.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in confirmGradeIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.GradeID) == "" {
			return errResult("grade_id is required"), nil, nil
		}
		v, err := b.ConfirmGrade(ctx, in.GradeID, actor, in.Note)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type triggerScheduleIn struct {
		Project string `json:"project" jsonschema:"project name"`
		Name    string `json:"name" jsonschema:"schedule name"`
		Force   bool   `json:"force,omitempty" jsonschema:"bypass max_active_runs / max_open_tasks backlog guard"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "trigger_schedule",
		Description: "Trigger a project's configured recurring schedule immediately.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in triggerScheduleIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" || strings.TrimSpace(in.Name) == "" {
			return errResult("project and name are required"), nil, nil
		}
		v, err := b.TriggerSchedule(ctx, in.Project, in.Name, in.Force)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type pauseScheduleIn struct {
		Project string `json:"project" jsonschema:"project name"`
		Name    string `json:"name" jsonschema:"schedule name"`
		Paused  bool   `json:"paused" jsonschema:"true to pause, false to resume"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "pause_schedule",
		Description: "Pause or resume a project's recurring schedule. A paused schedule triggers nothing and starts no runs for tasks it already queued.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pauseScheduleIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" || strings.TrimSpace(in.Name) == "" {
			return errResult("project and name are required"), nil, nil
		}
		if err := b.PauseSchedule(ctx, in.Project, in.Name, in.Paused); err != nil {
			return errResult(err.Error()), nil, nil
		}
		v, err := b.ListSchedules(ctx, in.Project)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})

	type updateLedgerIn struct {
		Project string `json:"project" jsonschema:"project name"`
		Key     string `json:"key,omitempty" jsonschema:"stable slug key (optional; derived from title if omitted)"`
		Title   string `json:"title" jsonschema:"concise title of the improvement item"`
		Status  string `json:"status" jsonschema:"proposed, pr_open, shipped, reverted, rejected, or wont_fix"`
		Summary string `json:"summary,omitempty" jsonschema:"optional rationale or context"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "update_project_ledger",
		Description: "Add or update an entry in a project's persistent improvement ledger (e.g. mark an idea wont_fix or rejected so future audits skip it).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateLedgerIn) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.Project) == "" || strings.TrimSpace(in.Title) == "" {
			return errResult("project and title are required"), nil, nil
		}
		v, err := b.UpsertProjectLedger(ctx, in.Project, engine.LedgerEntry{
			Key: in.Key, Title: in.Title, Status: in.Status, Summary: in.Summary,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := jsonText(v)
		return res, nil, err
	})
}

// LocalBackend serves MCP requests directly from the control plane's App.
type LocalBackend struct {
	App        *app.App
	Obj        objstore.Store
	OverviewFn func(ctx context.Context) (any, error)
	AnalyzeFn  func(yamlSrc string) (*resolve.Graph, []resolve.Issue)
}

func (l *LocalBackend) Overview(ctx context.Context) (any, error) {
	if l.OverviewFn != nil {
		return l.OverviewFn(ctx)
	}
	ops, err := l.App.Store.Operations(ctx)
	if err != nil {
		return nil, err
	}
	cfg := l.App.Cfg.Current()
	var projects []string
	for name := range cfg.Projects {
		projects = append(projects, name)
	}
	return map[string]any{"projects": projects, "operations": ops}, nil
}

func (l *LocalBackend) Attention(ctx context.Context, project string) (*app.AttentionReport, error) {
	return l.App.Attention(ctx, project)
}

func (l *LocalBackend) ListRuns(ctx context.Context, project, flowName, status string, limit int) ([]*store.Run, error) {
	f := store.RunFilter{Project: project, FlowName: flowName, Limit: limit}
	if status != "" {
		f.Statuses = []string{status}
	}
	return l.App.Store.ListRuns(ctx, f)
}

func (l *LocalBackend) ExplainRun(ctx context.Context, runID string) (*app.RunExplanation, error) {
	return l.App.ExplainRun(ctx, runID)
}

func (l *LocalBackend) InspectVisit(ctx context.Context, runID string, seq int) (map[string]any, error) {
	v, err := l.App.VisitDetail(ctx, runID, seq)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"visit": v}
	if v.TranscriptKey != "" && l.Obj != nil {
		if data, err := l.Obj.Get(ctx, v.TranscriptKey); err == nil {
			s := string(data)
			if len(s) > 8000 {
				s = "(truncated) …" + s[len(s)-8000:]
			}
			out["transcript_tail"] = s
		}
	}
	return out, nil
}

func (l *LocalBackend) ListFlows(ctx context.Context) ([]*store.FlowVersion, error) {
	return l.App.Store.ListFlows(ctx)
}

func (l *LocalBackend) GetFlow(ctx context.Context, name string, version int) (map[string]any, error) {
	fv, err := l.App.Store.GetFlow(ctx, name, version)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"flow": fv}
	if l.AnalyzeFn != nil {
		g, issues := l.AnalyzeFn(fv.YAML)
		res["graph"] = g
		res["issues"] = issues
	}
	if fv.TaskID != "" {
		if t, err := l.App.Store.GetTask(ctx, fv.TaskID); err == nil {
			res["task"] = t
		}
	}
	return res, nil
}

func (l *LocalBackend) ProjectSummary(ctx context.Context, project string) (*app.ProjectSummary, error) {
	return l.App.ProjectSummary(ctx, project)
}

func (l *LocalBackend) ProjectDigest(ctx context.Context, project string) (string, error) {
	s, err := l.App.ProjectSummary(ctx, project)
	if err != nil {
		return "", err
	}
	return app.RenderDigestMarkdown(s), nil
}

func (l *LocalBackend) GetRetro(ctx context.Context, runID string) (*app.Retro, error) {
	return l.App.GetRetro(ctx, runID)
}

func (l *LocalBackend) DecideGate(ctx context.Context, runID string, seq int, outcome, note, by string, extendVisits bool) error {
	if err := l.App.Engine.DecideWithOptions(ctx, runID, seq, outcome, by, note, extendVisits); err != nil {
		return err
	}
	l.App.Engine.Wake()
	return nil
}

func (l *LocalBackend) SteerRun(ctx context.Context, runID string, pause *bool, note *string, by string) (*store.Run, error) {
	return l.App.Engine.Steer(ctx, runID, pause, note, by)
}

func (l *LocalBackend) ResumeRun(ctx context.Context, runID, node, note, by string, flowVersion int, refresh bool) (*store.Run, error) {
	return l.App.Engine.ResumeOn(ctx, runID, flowVersion, refresh, node, note, by)
}

func (l *LocalBackend) CreateTask(ctx context.Context, project, title, body string, plan bool) (*store.Task, error) {
	t := &store.Task{Source: "mcp", Title: strings.TrimSpace(title), Body: body, Project: project}
	if err := l.App.CreateTask(ctx, t, plan); err != nil {
		return nil, err
	}
	return t, nil
}

func (l *LocalBackend) SendStoryToFlow(ctx context.Context, project, mapID, kind, id, release string, plan bool) (*store.Task, error) {
	return l.App.SendToFlow(ctx, project, mapID, kind, id, release, plan)
}

func (l *LocalBackend) StartRetro(ctx context.Context, runID, focus string) (*app.Retro, error) {
	return l.App.StartRetro(ctx, runID, focus)
}

func (l *LocalBackend) ImprovementSummary(ctx context.Context, project string) (*app.ImprovementSummary, error) {
	return l.App.ImprovementSummary(ctx, project)
}

func (l *LocalBackend) ListRunGrades(ctx context.Context, runID, project, rubric, blame string, limit int) ([]*store.RunGrade, error) {
	return l.App.Store.ListRunGrades(ctx, store.GradeFilter{RunID: runID, Project: project, Rubric: rubric, Blame: blame, Limit: limit})
}

func (l *LocalBackend) ListProposals(ctx context.Context, status string, limit int) ([]*store.ImprovementProposal, error) {
	return l.App.Store.ListProposals(ctx, store.ProposalFilter{Status: status, Limit: limit})
}

func (l *LocalBackend) GradeRun(ctx context.Context, runID string) (map[string]any, error) {
	if _, err := l.App.Store.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	started := l.App.StartGradeRun(runID)
	return map[string]any{"run_id": runID, "started": started, "grading": true}, nil
}

func (l *LocalBackend) RunImprover(ctx context.Context, project string, minFailures int) (*app.ImproverRunStatus, error) {
	return l.App.RunImprover(ctx, app.ImproverOptions{Project: project, MinFailures: minFailures, Trigger: "mcp"})
}

func (l *LocalBackend) DisagreeGrade(ctx context.Context, gradeID string, disagreed bool, by, note string) (*store.RunGrade, error) {
	return l.App.DisagreeGrade(ctx, gradeID, disagreed, by, note)
}

func (l *LocalBackend) ApplyProposal(ctx context.Context, proposalID, by, note, afterText string, force bool) (*store.ImprovementProposal, []string, error) {
	return l.App.ApplyProposal(ctx, proposalID, by, note, afterText, force)
}

func (l *LocalBackend) RejectProposal(ctx context.Context, proposalID, by, note string) (*store.ImprovementProposal, error) {
	return l.App.RejectProposal(ctx, proposalID, by, note)
}

func (l *LocalBackend) ConfirmGrade(ctx context.Context, gradeID, by, note string) (*store.RunGrade, error) {
	return l.App.ConfirmGrade(ctx, gradeID, by, note)
}

func (l *LocalBackend) ListSchedules(ctx context.Context, project string) ([]app.ScheduleView, error) {
	return l.App.ListSchedules(ctx, project), nil
}

func (l *LocalBackend) TriggerSchedule(ctx context.Context, project, name string, force bool) (*app.ScheduleState, error) {
	return l.App.TriggerSchedule(ctx, project, name, force)
}

func (l *LocalBackend) PauseSchedule(ctx context.Context, project, name string, paused bool) error {
	return l.App.PauseSchedule(ctx, project, name, paused)
}

func (l *LocalBackend) ProjectLedger(ctx context.Context, project string) ([]engine.LedgerEntry, error) {
	return l.App.Engine.Ledger(ctx, project), nil
}

func (l *LocalBackend) UpsertProjectLedger(ctx context.Context, project string, entry engine.LedgerEntry) ([]engine.LedgerEntry, error) {
	if err := l.App.Engine.UpsertLedger(ctx, project, entry); err != nil {
		return nil, err
	}
	return l.App.Engine.Ledger(ctx, project), nil
}

func (l *LocalBackend) ProjectBaselines(ctx context.Context, project string) (map[string]engine.BaselineEntry, error) {
	return l.App.Engine.Baselines(ctx, project), nil
}

func (l *LocalBackend) ExportCatalogImprovements(ctx context.Context) (string, error) {
	return l.App.ExportAppliedImprovements(ctx)
}
