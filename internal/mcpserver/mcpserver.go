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

	// Steering actions (only wired when Options.AllowSteer is true).
	DecideGate(ctx context.Context, runID string, seq int, outcome, note, by string, extendVisits bool) error
	SteerRun(ctx context.Context, runID string, pause *bool, note *string, by string) (*store.Run, error)
	ResumeRun(ctx context.Context, runID, node, note, by string, flowVersion int, refresh bool) (*store.Run, error)
	CreateTask(ctx context.Context, project, title, body string, plan bool) (*store.Task, error)
	SendStoryToFlow(ctx context.Context, project, mapID, kind, id, release string, plan bool) (*store.Task, error)
	StartRetro(ctx context.Context, runID, focus string) (*app.Retro, error)
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
