package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---- improvement loop: run facts, grades, proposals, prompt revisions ----

const (
	BlameAgent       = "agent"
	BlameEnvironment = "environment"

	ProposalPending  = "pending"
	ProposalApplied  = "applied"
	ProposalRejected = "rejected"
)

// RunFacts holds deterministic completion metrics and pinned prompt versions
// recorded for one finished run.
type RunFacts struct {
	RunID          string            `json:"run_id"`
	Project        string            `json:"project,omitempty"`
	FlowName       string            `json:"flow_name"`
	FlowVersion    int               `json:"flow_version"`
	Status         string            `json:"status"`
	WallMs         int64             `json:"wall_ms"`
	CostUSD        float64           `json:"cost_usd"`
	Tokens         int64             `json:"tokens"`
	Visits         int               `json:"visits"`
	LoopVisits     int               `json:"loop_visits"`
	MaxVisit       int               `json:"max_visit"`
	StepErrors     int               `json:"step_errors"`
	Resumes        int               `json:"resumes"`
	Gates          int               `json:"gates"`
	Steers         int               `json:"steers"`
	ExtraVisits    int               `json:"extra_visits"`
	PromptVersions map[string]string `json:"prompt_versions,omitempty"`
	CreatedAt      int64             `json:"created_at"`
	FinishedAt     int64             `json:"finished_at"`
}

// RunGrade is one rubric verdict for a finished run.
type RunGrade struct {
	ID             string            `json:"id"`
	RunID          string            `json:"run_id"`
	Project        string            `json:"project,omitempty"`
	FlowName       string            `json:"flow_name,omitempty"`
	Rubric         string            `json:"rubric"`
	RubricVersion  string            `json:"rubric_version"`
	Pass           bool              `json:"pass"`
	Blame          string            `json:"blame,omitempty"`       // "" | agent | environment
	Theme          string            `json:"theme,omitempty"`       // stable kebab-case failure slug
	TargetKind     string            `json:"target_kind,omitempty"` // preset | harness | project_agent | planner | project_planner | flow | infra
	Target         string            `json:"target,omitempty"`
	Node           string            `json:"node,omitempty"`
	Reason         string            `json:"reason"`
	PromptVersions map[string]string `json:"prompt_versions,omitempty"`
	Model          string            `json:"model,omitempty"`
	TokensIn       int64             `json:"tokens_in,omitempty"`
	TokensOut      int64             `json:"tokens_out,omitempty"`
	CostUSD        float64           `json:"cost_usd,omitempty"`
	Disagreed      bool              `json:"disagreed,omitempty"`
	DisagreedBy    string            `json:"disagreed_by,omitempty"`
	DisagreedNote  string            `json:"disagreed_note,omitempty"`
	DisagreedAt    int64             `json:"disagreed_at,omitempty"`
	ReviewedAt     int64             `json:"reviewed_at,omitempty"`
	CreatedAt      int64             `json:"created_at"`
}

// ImprovementProposal is an LLM-drafted, human-reviewed edit to a prompt or
// instruction block backed by repeated agent-blamed grade failures.
type ImprovementProposal struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"` // pending | applied | rejected
	TargetKind     string   `json:"target_kind"`
	Target         string   `json:"target"`
	Theme          string   `json:"theme,omitempty"`
	Title          string   `json:"title"`
	Reason         string   `json:"reason"`
	BeforeText     string   `json:"before_text"`
	AfterText      string   `json:"after_text"`
	BeforeHash     string   `json:"before_hash"`
	AfterHash      string   `json:"after_hash"`
	EvidenceRuns   []string `json:"evidence_runs"`
	EvidenceGrades []string `json:"evidence_grades"`
	Model          string   `json:"model,omitempty"`
	TokensIn       int64    `json:"tokens_in,omitempty"`
	TokensOut      int64    `json:"tokens_out,omitempty"`
	CostUSD        float64  `json:"cost_usd,omitempty"`
	DecidedBy      string   `json:"decided_by,omitempty"`
	DecidedAt      int64    `json:"decided_at,omitempty"`
	DecisionNote   string   `json:"decision_note,omitempty"`
	CreatedAt      int64    `json:"created_at"`
}

// PromptRevision marks a change to an editable prompt/instruction target so
// score trends can align performance against prompt versions.
type PromptRevision struct {
	ID          int64  `json:"id"`
	TargetKind  string `json:"target_kind"`
	Target      string `json:"target"`
	VersionHash string `json:"version_hash"`
	PrevHash    string `json:"prev_hash,omitempty"`
	Author      string `json:"author,omitempty"`
	Note        string `json:"note,omitempty"`
	ProposalID  string `json:"proposal_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

const runFactsCols = `run_id, project, flow_name, flow_version, status, wall_ms, cost_usd, tokens, visits, loop_visits, max_visit, step_errors, resumes, gates, steers, extra_visits, prompt_versions, created_at, finished_at`

func scanRunFacts(row interface{ Scan(...any) error }) (*RunFacts, error) {
	var f RunFacts
	var pv string
	err := row.Scan(&f.RunID, &f.Project, &f.FlowName, &f.FlowVersion, &f.Status, &f.WallMs, &f.CostUSD, &f.Tokens,
		&f.Visits, &f.LoopVisits, &f.MaxVisit, &f.StepErrors, &f.Resumes, &f.Gates, &f.Steers, &f.ExtraVisits,
		&pv, &f.CreatedAt, &f.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.PromptVersions = map[string]string{}
	if pv != "" && pv != "{}" {
		_ = json.Unmarshal([]byte(pv), &f.PromptVersions)
	}
	return &f, nil
}

func (s *Store) UpsertRunFacts(ctx context.Context, f *RunFacts) error {
	pv, _ := json.Marshal(f.PromptVersions)
	if len(pv) == 0 || string(pv) == "null" {
		pv = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO run_facts (`+runFactsCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET
			project = excluded.project,
			flow_name = excluded.flow_name,
			flow_version = excluded.flow_version,
			status = excluded.status,
			wall_ms = excluded.wall_ms,
			cost_usd = excluded.cost_usd,
			tokens = excluded.tokens,
			visits = excluded.visits,
			loop_visits = excluded.loop_visits,
			max_visit = excluded.max_visit,
			step_errors = excluded.step_errors,
			resumes = excluded.resumes,
			gates = excluded.gates,
			steers = excluded.steers,
			extra_visits = excluded.extra_visits,
			prompt_versions = excluded.prompt_versions,
			created_at = excluded.created_at,
			finished_at = excluded.finished_at`,
		f.RunID, f.Project, f.FlowName, f.FlowVersion, f.Status, f.WallMs, f.CostUSD, f.Tokens,
		f.Visits, f.LoopVisits, f.MaxVisit, f.StepErrors, f.Resumes, f.Gates, f.Steers, f.ExtraVisits,
		string(pv), f.CreatedAt, f.FinishedAt)
	return err
}

func (s *Store) GetRunFacts(ctx context.Context, runID string) (*RunFacts, error) {
	return scanRunFacts(s.db.QueryRowContext(ctx, `SELECT `+runFactsCols+` FROM run_facts WHERE run_id = ?`, runID))
}

func (s *Store) ListRunFacts(ctx context.Context, project string, limit int) ([]*RunFacts, error) {
	q := `SELECT ` + runFactsCols + ` FROM run_facts`
	var args []any
	if project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	if limit <= 0 {
		limit = 500
	}
	q += fmt.Sprintf(` ORDER BY finished_at DESC, created_at DESC LIMIT %d`, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RunFacts
	for rows.Next() {
		f, err := scanRunFacts(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

const runGradeCols = `id, run_id, project, flow_name, rubric, rubric_version, pass, blame, theme, target_kind, target, node, reason, prompt_versions, model, tokens_in, tokens_out, cost_usd, disagreed, disagreed_by, disagreed_note, disagreed_at, reviewed_at, created_at`

func scanRunGrade(row interface{ Scan(...any) error }) (*RunGrade, error) {
	var g RunGrade
	var passInt, disagreedInt int
	var pv string
	err := row.Scan(&g.ID, &g.RunID, &g.Project, &g.FlowName, &g.Rubric, &g.RubricVersion, &passInt,
		&g.Blame, &g.Theme, &g.TargetKind, &g.Target, &g.Node, &g.Reason, &pv,
		&g.Model, &g.TokensIn, &g.TokensOut, &g.CostUSD,
		&disagreedInt, &g.DisagreedBy, &g.DisagreedNote, &g.DisagreedAt, &g.ReviewedAt, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.Pass = passInt != 0
	g.Disagreed = disagreedInt != 0
	g.PromptVersions = map[string]string{}
	if pv != "" && pv != "{}" {
		_ = json.Unmarshal([]byte(pv), &g.PromptVersions)
	}
	return &g, nil
}

// UpsertRunGrades stores grades for one run atomically, replacing any earlier
// grades for the same (run_id, rubric) pair (e.g. on re-grade).
func (s *Store) UpsertRunGrades(ctx context.Context, grades []*RunGrade) error {
	if len(grades) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range grades {
		if g.CreatedAt == 0 {
			g.CreatedAt = now()
		}
		pv, _ := json.Marshal(g.PromptVersions)
		if len(pv) == 0 || string(pv) == "null" {
			pv = []byte("{}")
		}
		passInt := 0
		if g.Pass {
			passInt = 1
		}
		disInt := 0
		if g.Disagreed {
			disInt = 1
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO run_grades (`+runGradeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(run_id, rubric) DO UPDATE SET
				id = excluded.id,
				project = excluded.project,
				flow_name = excluded.flow_name,
				rubric_version = excluded.rubric_version,
				pass = excluded.pass,
				blame = excluded.blame,
				theme = excluded.theme,
				target_kind = excluded.target_kind,
				target = excluded.target,
				node = excluded.node,
				reason = excluded.reason,
				prompt_versions = excluded.prompt_versions,
				model = excluded.model,
				tokens_in = excluded.tokens_in,
				tokens_out = excluded.tokens_out,
				cost_usd = excluded.cost_usd,
				disagreed = 0,
				disagreed_by = '',
				disagreed_note = '',
				disagreed_at = 0,
				reviewed_at = 0,
				created_at = excluded.created_at`,
			g.ID, g.RunID, g.Project, g.FlowName, g.Rubric, g.RubricVersion, passInt,
			g.Blame, g.Theme, g.TargetKind, g.Target, g.Node, g.Reason, string(pv),
			g.Model, g.TokensIn, g.TokensOut, g.CostUSD,
			disInt, g.DisagreedBy, g.DisagreedNote, g.DisagreedAt, g.ReviewedAt, g.CreatedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetRunGrade(ctx context.Context, id string) (*RunGrade, error) {
	return scanRunGrade(s.db.QueryRowContext(ctx, `SELECT `+runGradeCols+` FROM run_grades WHERE id = ?`, id))
}

type GradeFilter struct {
	RunID      string
	Project    string
	Rubric     string
	Blame      string
	Pass       *bool
	Unreviewed bool
	Limit      int
}

func (s *Store) ListRunGrades(ctx context.Context, f GradeFilter) ([]*RunGrade, error) {
	var where []string
	var args []any
	if f.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, f.RunID)
	}
	if f.Project != "" {
		where = append(where, "project = ?")
		args = append(args, f.Project)
	}
	if f.Rubric != "" {
		where = append(where, "rubric = ?")
		args = append(args, f.Rubric)
	}
	if f.Blame != "" {
		where = append(where, "blame = ?")
		args = append(args, f.Blame)
	}
	if f.Pass != nil {
		if *f.Pass {
			where = append(where, "pass = 1")
		} else {
			where = append(where, "pass = 0")
		}
	}
	if f.Unreviewed {
		where = append(where, "reviewed_at = 0 AND disagreed = 0")
	}
	q := `SELECT ` + runGradeCols + ` FROM run_grades`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 500
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, rubric ASC LIMIT %d`, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RunGrade
	for rows.Next() {
		g, err := scanRunGrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) SetGradeDisagreement(ctx context.Context, id string, disagreed bool, by, note string) (*RunGrade, error) {
	disInt := 0
	var at int64
	if disagreed {
		disInt = 1
		at = now()
	} else {
		by, note = "", ""
	}
	res, err := s.db.ExecContext(ctx, `UPDATE run_grades SET disagreed = ?, disagreed_by = ?, disagreed_note = ?, disagreed_at = ? WHERE id = ?`,
		disInt, by, note, at, id)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetRunGrade(ctx, id)
}

func (s *Store) MarkGradesReviewed(ctx context.Context, ids []string, stamp int64) error {
	if len(ids) == 0 {
		return nil
	}
	if stamp == 0 {
		stamp = now()
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, stamp)
	for _, id := range ids {
		args = append(args, id)
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	_, err := s.db.ExecContext(ctx, `UPDATE run_grades SET reviewed_at = ? WHERE id IN (`+in+`)`, args...)
	return err
}

// GradedRunIDs returns the set of run IDs that already have at least one grade.
func (s *Store) GradedRunIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT run_id FROM run_grades`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ---- proposals ----

const proposalCols = `id, status, target_kind, target, theme, title, reason, before_text, after_text, before_hash, after_hash, evidence_runs, evidence_grades, model, tokens_in, tokens_out, cost_usd, decided_by, decided_at, decision_note, created_at`

func scanProposal(row interface{ Scan(...any) error }) (*ImprovementProposal, error) {
	var p ImprovementProposal
	var runsJSON, gradesJSON string
	err := row.Scan(&p.ID, &p.Status, &p.TargetKind, &p.Target, &p.Theme, &p.Title, &p.Reason,
		&p.BeforeText, &p.AfterText, &p.BeforeHash, &p.AfterHash, &runsJSON, &gradesJSON,
		&p.Model, &p.TokensIn, &p.TokensOut, &p.CostUSD,
		&p.DecidedBy, &p.DecidedAt, &p.DecisionNote, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.EvidenceRuns = []string{}
	p.EvidenceGrades = []string{}
	_ = json.Unmarshal([]byte(runsJSON), &p.EvidenceRuns)
	_ = json.Unmarshal([]byte(gradesJSON), &p.EvidenceGrades)
	return &p, nil
}

func (s *Store) CreateProposal(ctx context.Context, p *ImprovementProposal) error {
	if p.CreatedAt == 0 {
		p.CreatedAt = now()
	}
	if p.Status == "" {
		p.Status = ProposalPending
	}
	runsJSON, _ := json.Marshal(p.EvidenceRuns)
	if len(runsJSON) == 0 || string(runsJSON) == "null" {
		runsJSON = []byte("[]")
	}
	gradesJSON, _ := json.Marshal(p.EvidenceGrades)
	if len(gradesJSON) == 0 || string(gradesJSON) == "null" {
		gradesJSON = []byte("[]")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO improvement_proposals (`+proposalCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Status, p.TargetKind, p.Target, p.Theme, p.Title, p.Reason,
		p.BeforeText, p.AfterText, p.BeforeHash, p.AfterHash, string(runsJSON), string(gradesJSON),
		p.Model, p.TokensIn, p.TokensOut, p.CostUSD,
		p.DecidedBy, p.DecidedAt, p.DecisionNote, p.CreatedAt)
	return err
}

func (s *Store) GetProposal(ctx context.Context, id string) (*ImprovementProposal, error) {
	return scanProposal(s.db.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM improvement_proposals WHERE id = ?`, id))
}

type ProposalFilter struct {
	Status     string
	TargetKind string
	Target     string
	Limit      int
}

func (s *Store) ListProposals(ctx context.Context, f ProposalFilter) ([]*ImprovementProposal, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.TargetKind != "" {
		where = append(where, "target_kind = ?")
		args = append(args, f.TargetKind)
	}
	if f.Target != "" {
		where = append(where, "target = ?")
		args = append(args, f.Target)
	}
	q := `SELECT ` + proposalCols + ` FROM improvement_proposals`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ImprovementProposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DecideProposal(ctx context.Context, id, status, by, note, afterText, afterHash string) (*ImprovementProposal, error) {
	at := now()
	var res sql.Result
	var err error
	if afterText != "" {
		res, err = s.db.ExecContext(ctx, `UPDATE improvement_proposals SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, after_text = ?, after_hash = ? WHERE id = ? AND status = ?`,
			status, by, at, note, afterText, afterHash, id, ProposalPending)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE improvement_proposals SET status = ?, decided_by = ?, decided_at = ?, decision_note = ? WHERE id = ? AND status = ?`,
			status, by, at, note, id, ProposalPending)
	}
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		if _, err := s.GetProposal(ctx, id); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("proposal %s is no longer pending", id)
	}
	return s.GetProposal(ctx, id)
}

// ---- prompt revisions ----

func (s *Store) AddPromptRevision(ctx context.Context, r *PromptRevision) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO prompt_revisions (target_kind, target, version_hash, prev_hash, author, note, proposal_id, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		r.TargetKind, r.Target, r.VersionHash, r.PrevHash, r.Author, r.Note, r.ProposalID, r.CreatedAt)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) ListPromptRevisions(ctx context.Context, targetKind, target string, limit int) ([]*PromptRevision, error) {
	var where []string
	var args []any
	if targetKind != "" {
		where = append(where, "target_kind = ?")
		args = append(args, targetKind)
	}
	if target != "" {
		where = append(where, "target = ?")
		args = append(args, target)
	}
	q := `SELECT id, target_kind, target, version_hash, prev_hash, author, note, proposal_id, created_at FROM prompt_revisions`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	if limit <= 0 {
		limit = 200
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT %d`, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PromptRevision
	for rows.Next() {
		var r PromptRevision
		if err := rows.Scan(&r.ID, &r.TargetKind, &r.Target, &r.VersionHash, &r.PrevHash, &r.Author, &r.Note, &r.ProposalID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
