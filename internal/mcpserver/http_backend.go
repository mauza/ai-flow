package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/store"
)

// HTTPBackend implements Backend against a running ai-flow HTTP API, powering
// `ai-flow mcp -url http://ai-flow:8080` for local stdio MCP clients.
type HTTPBackend struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func NewHTTPBackend(baseURL, token string) *HTTPBackend {
	return &HTTPBackend{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *HTTPBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(data) == 0 {
		return nil
	}
	if sp, ok := out.(*string); ok {
		*sp = string(data)
		return nil
	}
	return json.Unmarshal(data, out)
}

func (h *HTTPBackend) Overview(ctx context.Context) (any, error) {
	var out map[string]any
	return out, h.do(ctx, http.MethodGet, "/api/overview", nil, &out)
}

func (h *HTTPBackend) Attention(ctx context.Context, project string) (*app.AttentionReport, error) {
	p := "/api/attention"
	if project != "" {
		p += "?project=" + url.QueryEscape(project)
	}
	var out app.AttentionReport
	return &out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) ListRuns(ctx context.Context, project, flowName, status string, limit int) ([]*store.Run, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if flowName != "" {
		q.Set("flow", flowName)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	p := "/api/runs"
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	var out []*store.Run
	return out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) ExplainRun(ctx context.Context, runID string) (*app.RunExplanation, error) {
	var out app.RunExplanation
	return &out, h.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(runID)+"/explain", nil, &out)
}

func (h *HTTPBackend) InspectVisit(ctx context.Context, runID string, seq int) (map[string]any, error) {
	var rv struct {
		Visits []*store.Visit `json:"visits"`
	}
	if err := h.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(runID), nil, &rv); err != nil {
		return nil, err
	}
	var found *store.Visit
	for _, v := range rv.Visits {
		if v.Seq == seq {
			found = v
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("visit %d not found on run %s", seq, runID)
	}
	out := map[string]any{"visit": found}
	if found.TranscriptKey != "" {
		var tr string
		if err := h.do(ctx, http.MethodGet, fmt.Sprintf("/api/runs/%s/visits/%d/transcript", url.PathEscape(runID), seq), nil, &tr); err == nil {
			if len(tr) > 8000 {
				tr = "(truncated) …" + tr[len(tr)-8000:]
			}
			out["transcript_tail"] = tr
		}
	}
	return out, nil
}

func (h *HTTPBackend) ListFlows(ctx context.Context) ([]*store.FlowVersion, error) {
	var out []*store.FlowVersion
	return out, h.do(ctx, http.MethodGet, "/api/flows", nil, &out)
}

func (h *HTTPBackend) GetFlow(ctx context.Context, name string, version int) (map[string]any, error) {
	p := "/api/flows/" + url.PathEscape(name)
	if version > 0 {
		p += "/versions/" + strconv.Itoa(version)
	}
	var out map[string]any
	return out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) ProjectSummary(ctx context.Context, project string) (*app.ProjectSummary, error) {
	var out app.ProjectSummary
	return &out, h.do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/summary", nil, &out)
}

func (h *HTTPBackend) ProjectDigest(ctx context.Context, project string) (string, error) {
	var out string
	return out, h.do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/digest?format=md", nil, &out)
}

func (h *HTTPBackend) GetRetro(ctx context.Context, runID string) (*app.Retro, error) {
	var out struct {
		Retro *app.Retro `json:"retro"`
	}
	if err := h.do(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(runID)+"/retro", nil, &out); err != nil {
		return nil, err
	}
	return out.Retro, nil
}

func (h *HTTPBackend) DecideGate(ctx context.Context, runID string, seq int, outcome, note, by string, extendVisits bool) error {
	body := map[string]any{"outcome": outcome, "note": note, "by": by}
	if extendVisits {
		body["extend_visits"] = true
	}
	return h.do(ctx, http.MethodPost, fmt.Sprintf("/api/runs/%s/gates/%d", url.PathEscape(runID), seq), body, nil)
}

func (h *HTTPBackend) SteerRun(ctx context.Context, runID string, pause *bool, note *string, by string) (*store.Run, error) {
	body := map[string]any{"by": by}
	if pause != nil {
		body["pause"] = *pause
	}
	if note != nil {
		body["note"] = *note
	}
	var out store.Run
	return &out, h.do(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(runID)+"/steer", body, &out)
}

func (h *HTTPBackend) ResumeRun(ctx context.Context, runID, node, note, by string, flowVersion int, refresh bool) (*store.Run, error) {
	body := map[string]any{"node": node, "note": note, "by": by}
	if flowVersion > 0 {
		body["flow_version"] = flowVersion
	}
	if refresh {
		body["refresh"] = true
	}
	var out store.Run
	return &out, h.do(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(runID)+"/resume", body, &out)
}

func (h *HTTPBackend) CreateTask(ctx context.Context, project, title, body string, plan bool) (*store.Task, error) {
	var out store.Task
	return &out, h.do(ctx, http.MethodPost, "/api/tasks", map[string]any{
		"project": project, "title": title, "body": body, "plan": plan,
	}, &out)
}

func (h *HTTPBackend) SendStoryToFlow(ctx context.Context, project, mapID, kind, id, release string, plan bool) (*store.Task, error) {
	var out store.Task
	return &out, h.do(ctx, http.MethodPost, fmt.Sprintf("/api/projects/%s/maps/%s/send", url.PathEscape(project), url.PathEscape(mapID)), map[string]any{
		"kind": kind, "id": id, "release": release, "plan": plan,
	}, &out)
}

func (h *HTTPBackend) StartRetro(ctx context.Context, runID, focus string) (*app.Retro, error) {
	var out struct {
		Retro *app.Retro `json:"retro"`
	}
	if err := h.do(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(runID)+"/retro", map[string]any{"note": focus}, &out); err != nil {
		return nil, err
	}
	return out.Retro, nil
}

func (h *HTTPBackend) ImprovementSummary(ctx context.Context, project string) (*app.ImprovementSummary, error) {
	p := "/api/improvement/summary"
	if project != "" {
		p += "?project=" + url.QueryEscape(project)
	}
	var out app.ImprovementSummary
	return &out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) ListRunGrades(ctx context.Context, runID, project, rubric, blame string, limit int) ([]*store.RunGrade, error) {
	q := url.Values{}
	if runID != "" {
		q.Set("run", runID)
	}
	if project != "" {
		q.Set("project", project)
	}
	if rubric != "" {
		q.Set("rubric", rubric)
	}
	if blame != "" {
		q.Set("blame", blame)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	p := "/api/improvement/grades"
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	var out []*store.RunGrade
	return out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) ListProposals(ctx context.Context, status string, limit int) ([]*store.ImprovementProposal, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	p := "/api/improvement/proposals"
	if enc := q.Encode(); enc != "" {
		p += "?" + enc
	}
	var out []*store.ImprovementProposal
	return out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) GradeRun(ctx context.Context, runID string) (map[string]any, error) {
	var out map[string]any
	return out, h.do(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(runID)+"/grade", nil, &out)
}

func (h *HTTPBackend) RunImprover(ctx context.Context, project string, minFailures int) (*app.ImproverRunStatus, error) {
	body := map[string]any{}
	if project != "" {
		body["project"] = project
	}
	if minFailures > 0 {
		body["min_failures"] = minFailures
	}
	var out app.ImproverRunStatus
	return &out, h.do(ctx, http.MethodPost, "/api/improvement/run", body, &out)
}

func (h *HTTPBackend) DisagreeGrade(ctx context.Context, gradeID string, disagreed bool, by, note string) (*store.RunGrade, error) {
	var out store.RunGrade
	return &out, h.do(ctx, http.MethodPost, "/api/improvement/grades/"+url.PathEscape(gradeID)+"/disagree", map[string]any{
		"disagreed": disagreed, "by": by, "note": note,
	}, &out)
}

func (h *HTTPBackend) ApplyProposal(ctx context.Context, proposalID, by, note, afterText string, force bool) (*store.ImprovementProposal, []string, error) {
	var out struct {
		Proposal *store.ImprovementProposal `json:"proposal"`
		Warnings []string                   `json:"warnings"`
	}
	err := h.do(ctx, http.MethodPost, "/api/improvement/proposals/"+url.PathEscape(proposalID)+"/apply", map[string]any{
		"by": by, "note": note, "after_text": afterText, "force": force,
	}, &out)
	return out.Proposal, out.Warnings, err
}

func (h *HTTPBackend) RejectProposal(ctx context.Context, proposalID, by, note string) (*store.ImprovementProposal, error) {
	var out struct {
		Proposal *store.ImprovementProposal `json:"proposal"`
	}
	err := h.do(ctx, http.MethodPost, "/api/improvement/proposals/"+url.PathEscape(proposalID)+"/reject", map[string]any{
		"by": by, "note": note,
	}, &out)
	return out.Proposal, err
}

func (h *HTTPBackend) ConfirmGrade(ctx context.Context, gradeID, by, note string) (*store.RunGrade, error) {
	var out store.RunGrade
	return &out, h.do(ctx, http.MethodPost, "/api/improvement/grades/"+url.PathEscape(gradeID)+"/confirm", map[string]any{
		"by": by, "note": note,
	}, &out)
}

func (h *HTTPBackend) ListSchedules(ctx context.Context, project string) ([]app.ScheduleView, error) {
	p := "/api/schedules"
	if project != "" {
		p = "/api/projects/" + url.PathEscape(project) + "/schedules"
	}
	var out []app.ScheduleView
	return out, h.do(ctx, http.MethodGet, p, nil, &out)
}

func (h *HTTPBackend) TriggerSchedule(ctx context.Context, project, name string, force bool) (*app.ScheduleState, error) {
	var out app.ScheduleState
	return &out, h.do(ctx, http.MethodPost, fmt.Sprintf("/api/projects/%s/schedules/%s/run", url.PathEscape(project), url.PathEscape(name)), map[string]any{"force": force}, &out)
}

func (h *HTTPBackend) PauseSchedule(ctx context.Context, project, name string, paused bool) error {
	return h.do(ctx, http.MethodPost, fmt.Sprintf("/api/projects/%s/schedules/%s/pause", url.PathEscape(project), url.PathEscape(name)), map[string]any{"paused": paused}, nil)
}

func (h *HTTPBackend) ProjectLedger(ctx context.Context, project string) ([]engine.LedgerEntry, error) {
	var out []engine.LedgerEntry
	return out, h.do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/ledger", nil, &out)
}

func (h *HTTPBackend) UpsertProjectLedger(ctx context.Context, project string, entry engine.LedgerEntry) ([]engine.LedgerEntry, error) {
	var out []engine.LedgerEntry
	return out, h.do(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(project)+"/ledger", entry, &out)
}

func (h *HTTPBackend) ProjectBaselines(ctx context.Context, project string) (map[string]engine.BaselineEntry, error) {
	var out map[string]engine.BaselineEntry
	return out, h.do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(project)+"/baselines", nil, &out)
}

func (h *HTTPBackend) ExportCatalogImprovements(ctx context.Context) (string, error) {
	var out string
	return out, h.do(ctx, http.MethodGet, "/api/improvement/export-catalog", nil, &out)
}
