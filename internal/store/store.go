// Package store persists tasks, flow versions, runs and node visits in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	source TEXT NOT NULL,
	external_id TEXT NOT NULL DEFAULT '',
	identifier TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL DEFAULT '',
	project TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	flow_name TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_external ON tasks(source, external_id) WHERE external_id != '';

CREATE TABLE IF NOT EXISTS flow_versions (
	name TEXT NOT NULL,
	version INTEGER NOT NULL,
	yaml TEXT NOT NULL,
	project TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (name, version)
);

CREATE TABLE IF NOT EXISTS runs (
	id TEXT PRIMARY KEY,
	flow_name TEXT NOT NULL,
	flow_version INTEGER NOT NULL,
	task_id TEXT NOT NULL DEFAULT '',
	project TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	current_node TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL DEFAULT '',
	base TEXT NOT NULL DEFAULT '',
	pr_url TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	diff TEXT NOT NULL DEFAULT '{}',
	cost_usd REAL NOT NULL DEFAULT 0,
	tokens INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS runs_status ON runs(status);

CREATE TABLE IF NOT EXISTS visits (
	run_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	node TEXT NOT NULL,
	visit INTEGER NOT NULL,
	type TEXT NOT NULL,
	status TEXT NOT NULL,
	outcome TEXT NOT NULL DEFAULT '',
	outputs TEXT NOT NULL DEFAULT '{}',
	summary TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '',
	job_name TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	tokens_in INTEGER NOT NULL DEFAULT 0,
	tokens_out INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	llm_calls INTEGER NOT NULL DEFAULT 0,
	transcript_key TEXT NOT NULL DEFAULT '',
	log_tail TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	progress TEXT NOT NULL DEFAULT '',
	decided_by TEXT NOT NULL DEFAULT '',
	deadline INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	flow_name TEXT NOT NULL DEFAULT '',
	node TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	message TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_run ON events(run_id);
CREATE INDEX IF NOT EXISTS events_flow ON events(flow_name);

CREATE TABLE IF NOT EXISTS chat (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	flow_name TEXT NOT NULL,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	yaml TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_flow ON chat(flow_name);

CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps busy errors away
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().UnixMilli() }

// Time converts a stored millisecond timestamp.
func Time(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	t := time.UnixMilli(ms)
	return &t
}

// ---- tasks ----

const (
	TaskNew        = "new"
	TaskPlanning   = "planning"
	TaskPlanFailed = "plan_failed"
	TaskFlowReady  = "flow_ready"
	TaskRunning    = "running"
	TaskSucceeded  = "succeeded"
	TaskFailed     = "failed"
)

type Task struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url,omitempty"`
	Project    string `json:"project"`
	Status     string `json:"status"`
	FlowName   string `json:"flow_name,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

const taskCols = `id, source, external_id, identifier, title, body, url, project, status, flow_name, error, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Source, &t.ExternalID, &t.Identifier, &t.Title, &t.Body, &t.URL, &t.Project, &t.Status, &t.FlowName, &t.Error, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

func (s *Store) CreateTask(ctx context.Context, t *Task) error {
	t.CreatedAt, t.UpdatedAt = now(), now()
	if t.Status == "" {
		t.Status = TaskNew
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (`+taskCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Source, t.ExternalID, t.Identifier, t.Title, t.Body, t.URL, t.Project, t.Status, t.FlowName, t.Error, t.CreatedAt, t.UpdatedAt)
	return err
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

func (s *Store) TaskByExternal(ctx context.Context, source, externalID string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE source = ? AND external_id = ?`, source, externalID))
}

func (s *Store) ListTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTask(ctx context.Context, id string, fields map[string]any) error {
	return s.update(ctx, "tasks", "id = ?", []any{id}, fields, true)
}

func (s *Store) DeleteTask(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	return err
}

// ---- flows ----

type FlowVersion struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	YAML      string `json:"yaml"`
	Project   string `json:"project,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// SaveFlow stores yaml as the next version of name and returns that version.
func (s *Store) SaveFlow(ctx context.Context, fv *FlowVersion) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM flow_versions WHERE name = ?`, fv.Name).Scan(&v); err != nil {
		return 0, err
	}
	fv.Version, fv.CreatedAt = v+1, now()
	_, err = tx.ExecContext(ctx, `INSERT INTO flow_versions (name, version, yaml, project, task_id, created_by, note, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		fv.Name, fv.Version, fv.YAML, fv.Project, fv.TaskID, fv.CreatedBy, fv.Note, fv.CreatedAt)
	if err != nil {
		return 0, err
	}
	return fv.Version, tx.Commit()
}

const flowCols = `name, version, yaml, project, task_id, created_by, note, created_at`

func scanFlow(row interface{ Scan(...any) error }) (*FlowVersion, error) {
	var f FlowVersion
	err := row.Scan(&f.Name, &f.Version, &f.YAML, &f.Project, &f.TaskID, &f.CreatedBy, &f.Note, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &f, err
}

// GetFlow returns a version; version 0 means latest.
func (s *Store) GetFlow(ctx context.Context, name string, version int) (*FlowVersion, error) {
	if version == 0 {
		return scanFlow(s.db.QueryRowContext(ctx, `SELECT `+flowCols+` FROM flow_versions WHERE name = ? ORDER BY version DESC LIMIT 1`, name))
	}
	return scanFlow(s.db.QueryRowContext(ctx, `SELECT `+flowCols+` FROM flow_versions WHERE name = ? AND version = ?`, name, version))
}

func (s *Store) FlowVersions(ctx context.Context, name string) ([]*FlowVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, version, '', project, task_id, created_by, note, created_at FROM flow_versions WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FlowVersion
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFlows returns the latest version of every flow (without yaml).
func (s *Store) ListFlows(ctx context.Context) ([]*FlowVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.name, f.version, '', f.project, f.task_id, f.created_by, f.note, f.created_at
		FROM flow_versions f JOIN (SELECT name, MAX(version) v FROM flow_versions GROUP BY name) m
		ON f.name = m.name AND f.version = m.v ORDER BY f.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*FlowVersion
	for rows.Next() {
		f, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) FlowExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_versions WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// ---- runs ----

const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunWaiting   = "waiting" // a gate needs a human
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCanceled  = "canceled"
)

func RunDone(status string) bool {
	return status == RunSucceeded || status == RunFailed || status == RunCanceled
}

type Run struct {
	ID          string          `json:"id"`
	FlowName    string          `json:"flow_name"`
	FlowVersion int             `json:"flow_version"`
	TaskID      string          `json:"task_id,omitempty"`
	Project     string          `json:"project,omitempty"`
	Status      string          `json:"status"`
	CurrentNode string          `json:"current_node,omitempty"`
	Branch      string          `json:"branch"`
	Base        string          `json:"base"`
	PRURL       string          `json:"pr_url,omitempty"`
	Error       string          `json:"error,omitempty"`
	Diff        json.RawMessage `json:"diff"`
	CostUSD     float64         `json:"cost_usd"`
	Tokens      int64           `json:"tokens"`
	CreatedAt   int64           `json:"created_at"`
	StartedAt   int64           `json:"started_at,omitempty"`
	FinishedAt  int64           `json:"finished_at,omitempty"`
}

const runCols = `id, flow_name, flow_version, task_id, project, status, current_node, branch, base, pr_url, error, diff, cost_usd, tokens, created_at, started_at, finished_at`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var diff string
	err := row.Scan(&r.ID, &r.FlowName, &r.FlowVersion, &r.TaskID, &r.Project, &r.Status, &r.CurrentNode, &r.Branch, &r.Base, &r.PRURL, &r.Error, &diff, &r.CostUSD, &r.Tokens, &r.CreatedAt, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	r.Diff = json.RawMessage(diff)
	return &r, err
}

func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	r.CreatedAt = now()
	if len(r.Diff) == 0 {
		r.Diff = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.FlowName, r.FlowVersion, r.TaskID, r.Project, r.Status, r.CurrentNode, r.Branch, r.Base, r.PRURL, r.Error, string(r.Diff), r.CostUSD, r.Tokens, r.CreatedAt, r.StartedAt, r.FinishedAt)
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

type RunFilter struct {
	Statuses []string
	FlowName string
	TaskID   string
	Limit    int
}

func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	var where []string
	var args []any
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Statuses)), ",")+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.FlowName != "" {
		where = append(where, "flow_name = ?")
		args = append(args, f.FlowName)
	}
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	q := `SELECT ` + runCols + ` FROM runs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit == 0 {
		f.Limit = 200
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, rowid DESC LIMIT %d", f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRun(ctx context.Context, id string, fields map[string]any) error {
	return s.update(ctx, "runs", "id = ?", []any{id}, fields, false)
}

// ---- visits ----

const (
	VisitPending   = "pending"
	VisitRunning   = "running"
	VisitWaiting   = "waiting"
	VisitSucceeded = "succeeded"
	VisitError     = "error"
	VisitCanceled  = "canceled"
)

type Visit struct {
	RunID         string          `json:"run_id"`
	Seq           int             `json:"seq"`
	Node          string          `json:"node"`
	Visit         int             `json:"visit"`
	Type          string          `json:"type"`
	Status        string          `json:"status"`
	Outcome       string          `json:"outcome,omitempty"`
	Outputs       json.RawMessage `json:"outputs"`
	Summary       string          `json:"summary,omitempty"`
	Error         string          `json:"error,omitempty"`
	Prompt        string          `json:"prompt,omitempty"`
	JobName       string          `json:"job_name,omitempty"`
	Model         string          `json:"model,omitempty"`
	TokensIn      int64           `json:"tokens_in"`
	TokensOut     int64           `json:"tokens_out"`
	CostUSD       float64         `json:"cost_usd"`
	LLMCalls      int64           `json:"llm_calls"`
	TranscriptKey string          `json:"transcript_key,omitempty"`
	LogTail       string          `json:"log_tail,omitempty"`
	CommitSHA     string          `json:"commit_sha,omitempty"`
	Progress      string          `json:"progress,omitempty"`
	DecidedBy     string          `json:"decided_by,omitempty"`
	Deadline      int64           `json:"deadline,omitempty"`
	CreatedAt     int64           `json:"created_at"`
	StartedAt     int64           `json:"started_at,omitempty"`
	FinishedAt    int64           `json:"finished_at,omitempty"`
}

const visitCols = `run_id, seq, node, visit, type, status, outcome, outputs, summary, error, prompt, job_name, model, tokens_in, tokens_out, cost_usd, llm_calls, transcript_key, log_tail, commit_sha, progress, decided_by, deadline, created_at, started_at, finished_at`

func scanVisit(row interface{ Scan(...any) error }) (*Visit, error) {
	var v Visit
	var outputs string
	err := row.Scan(&v.RunID, &v.Seq, &v.Node, &v.Visit, &v.Type, &v.Status, &v.Outcome, &outputs, &v.Summary, &v.Error, &v.Prompt, &v.JobName, &v.Model,
		&v.TokensIn, &v.TokensOut, &v.CostUSD, &v.LLMCalls, &v.TranscriptKey, &v.LogTail, &v.CommitSHA, &v.Progress, &v.DecidedBy, &v.Deadline, &v.CreatedAt, &v.StartedAt, &v.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	v.Outputs = json.RawMessage(outputs)
	return &v, err
}

// AddVisit appends the next visit of node to the run and returns it.
func (s *Store) AddVisit(ctx context.Context, runID, node, typ string) (*Visit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var seq, visit int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM visits WHERE run_id = ?`, runID).Scan(&seq); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM visits WHERE run_id = ? AND node = ?`, runID, node).Scan(&visit); err != nil {
		return nil, err
	}
	v := &Visit{RunID: runID, Seq: seq + 1, Node: node, Visit: visit + 1, Type: typ, Status: VisitPending, Outputs: json.RawMessage("{}"), CreatedAt: now()}
	_, err = tx.ExecContext(ctx, `INSERT INTO visits (run_id, seq, node, visit, type, status, outputs, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		v.RunID, v.Seq, v.Node, v.Visit, v.Type, v.Status, "{}", v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return v, tx.Commit()
}

func (s *Store) GetVisit(ctx context.Context, runID string, seq int) (*Visit, error) {
	return scanVisit(s.db.QueryRowContext(ctx, `SELECT `+visitCols+` FROM visits WHERE run_id = ? AND seq = ?`, runID, seq))
}

func (s *Store) Visits(ctx context.Context, runID string) ([]*Visit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+visitCols+` FROM visits WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Visit
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LastVisit returns the run's most recent visit, or ErrNotFound.
func (s *Store) LastVisit(ctx context.Context, runID string) (*Visit, error) {
	return scanVisit(s.db.QueryRowContext(ctx, `SELECT `+visitCols+` FROM visits WHERE run_id = ? ORDER BY seq DESC LIMIT 1`, runID))
}

func (s *Store) UpdateVisit(ctx context.Context, runID string, seq int, fields map[string]any) error {
	return s.update(ctx, "visits", "run_id = ? AND seq = ?", []any{runID, seq}, fields, false)
}

// UpdateVisitIf updates a visit only while its status is one of statuses.
// It reports whether the update happened.
func (s *Store) UpdateVisitIf(ctx context.Context, runID string, seq int, statuses []string, fields map[string]any) (bool, error) {
	where := "run_id = ? AND seq = ? AND status IN (" + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + ")"
	args := []any{runID, seq}
	for _, st := range statuses {
		args = append(args, st)
	}
	err := s.update(ctx, "visits", where, args, fields, false)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// AddUsage adds LLM usage to a visit and its run.
func (s *Store) AddUsage(ctx context.Context, runID string, seq int, model string, in, out int64, cost float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE visits SET tokens_in = tokens_in + ?, tokens_out = tokens_out + ?, cost_usd = cost_usd + ?, llm_calls = llm_calls + 1, model = ? WHERE run_id = ? AND seq = ?`,
		in, out, cost, model, runID, seq); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET tokens = tokens + ?, cost_usd = cost_usd + ? WHERE id = ?`, in+out, cost, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- events ----

type Event struct {
	ID        int64  `json:"id"`
	RunID     string `json:"run_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	FlowName  string `json:"flow_name,omitempty"`
	Node      string `json:"node,omitempty"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) AddEvent(ctx context.Context, e *Event) error {
	e.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (run_id, task_id, flow_name, node, kind, message, created_at) VALUES (?,?,?,?,?,?,?)`,
		e.RunID, e.TaskID, e.FlowName, e.Node, e.Kind, e.Message, e.CreatedAt)
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) Events(ctx context.Context, column, value string, limit int) ([]*Event, error) {
	switch column {
	case "run_id", "task_id", "flow_name":
	default:
		return nil, fmt.Errorf("bad column %q", column)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, task_id, flow_name, node, kind, message, created_at FROM events WHERE `+column+` = ? ORDER BY id DESC LIMIT ?`, value, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.TaskID, &e.FlowName, &e.Node, &e.Kind, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---- chat ----

type ChatMessage struct {
	ID        int64  `json:"id"`
	FlowName  string `json:"flow_name"`
	Role      string `json:"role"` // user | assistant | system
	Content   string `json:"content"`
	YAML      string `json:"yaml,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) AddChat(ctx context.Context, m *ChatMessage) error {
	m.CreatedAt = now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO chat (flow_name, role, content, yaml, created_at) VALUES (?,?,?,?,?)`, m.FlowName, m.Role, m.Content, m.YAML, m.CreatedAt)
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) Chat(ctx context.Context, flowName string) ([]*ChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, flow_name, role, content, yaml, created_at FROM chat WHERE flow_name = ? ORDER BY id`, flowName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.FlowName, &m.Role, &m.Content, &m.YAML, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ---- kv ----

func (s *Store) GetKV(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- helpers ----

func (s *Store) update(ctx context.Context, table, where string, whereArgs []any, fields map[string]any, touch bool) error {
	if len(fields) == 0 {
		return nil
	}
	var sets []string
	var args []any
	for k, v := range fields {
		if !validColumn(k) {
			return fmt.Errorf("bad column %q", k)
		}
		if raw, ok := v.(json.RawMessage); ok {
			v = string(raw)
		}
		sets = append(sets, k+" = ?")
		args = append(args, v)
	}
	if touch {
		sets = append(sets, "updated_at = ?")
		args = append(args, now())
	}
	args = append(args, whereArgs...)
	res, err := s.db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, strings.Join(sets, ", "), where), args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func validColumn(c string) bool {
	for _, r := range c {
		if !(r == '_' || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return c != ""
}

// Now is exposed for callers that stamp times consistently with the store.
func Now() int64 { return now() }
