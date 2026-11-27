package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Version 0 is the original schema. Migrations and their version are committed
// together, so opening an existing database is safe across interruptions.
func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 8 {
		return fmt.Errorf("unsupported database version %d", version)
	}
	if version == 0 {
		if _, err := tx.Exec(`ALTER TABLE runs ADD COLUMN snapshot TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS runs_project_created ON runs(project, created_at);
		PRAGMA user_version = 1;`); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err := tx.Exec(`ALTER TABLE visits ADD COLUMN launch_state TEXT NOT NULL DEFAULT ''; PRAGMA user_version = 2;`); err != nil {
			return err
		}
	}
	if version < 3 {
		// A resume re-opens a failed run: visits up to resume_seq no longer count
		// toward max_visits, so each resume gets one fresh bounded window.
		if _, err := tx.Exec(`ALTER TABLE runs ADD COLUMN resume_seq INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE runs ADD COLUMN resumes INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE runs ADD COLUMN resume_note TEXT NOT NULL DEFAULT '';
		PRAGMA user_version = 3;`); err != nil {
			return err
		}
	}
	if version < 4 {
		// Parallel branches: a branch visit names the parallel visit it belongs to
		// (fork_seq) and its branch (lane). The main path has fork_seq 0.
		if _, err := tx.Exec(`ALTER TABLE visits ADD COLUMN fork_seq INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE visits ADD COLUMN lane TEXT NOT NULL DEFAULT '';
		PRAGMA user_version = 4;`); err != nil {
			return err
		}
	}
	if version < 5 {
		// Prompt tokens the upstream served from its cache (a subset of tokens_in).
		if _, err := tx.Exec(`ALTER TABLE visits ADD COLUMN tokens_cached INTEGER NOT NULL DEFAULT 0; PRAGMA user_version = 5;`); err != nil {
			return err
		}
	}
	if version < 6 {
		// Mid-flight human steering (pause after current step, steering note)
		// and human-granted extra loop rounds at gates.
		if _, err := tx.Exec(`ALTER TABLE runs ADD COLUMN pause_requested INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE runs ADD COLUMN steer_note TEXT NOT NULL DEFAULT '';
		ALTER TABLE runs ADD COLUMN steer_seq INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE runs ADD COLUMN extra_visits INTEGER NOT NULL DEFAULT 0;
		PRAGMA user_version = 6;`); err != nil {
			return err
		}
	}
	if version < 7 {
		// Closed-loop improvement: deterministic run facts with pinned prompt
		// versions, automatic rubric grades with agent/environment blame and
		// human disagreement, prompt edit proposals, and prompt revision markers.
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS run_facts (
			run_id TEXT PRIMARY KEY,
			project TEXT NOT NULL DEFAULT '',
			flow_name TEXT NOT NULL DEFAULT '',
			flow_version INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT '',
			wall_ms INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			tokens INTEGER NOT NULL DEFAULT 0,
			visits INTEGER NOT NULL DEFAULT 0,
			loop_visits INTEGER NOT NULL DEFAULT 0,
			max_visit INTEGER NOT NULL DEFAULT 0,
			step_errors INTEGER NOT NULL DEFAULT 0,
			resumes INTEGER NOT NULL DEFAULT 0,
			gates INTEGER NOT NULL DEFAULT 0,
			steers INTEGER NOT NULL DEFAULT 0,
			extra_visits INTEGER NOT NULL DEFAULT 0,
			prompt_versions TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL DEFAULT 0,
			finished_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS run_facts_project_finished ON run_facts(project, finished_at);

		CREATE TABLE IF NOT EXISTS run_grades (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			flow_name TEXT NOT NULL DEFAULT '',
			rubric TEXT NOT NULL,
			rubric_version TEXT NOT NULL DEFAULT '',
			pass INTEGER NOT NULL DEFAULT 0,
			blame TEXT NOT NULL DEFAULT '',
			theme TEXT NOT NULL DEFAULT '',
			target_kind TEXT NOT NULL DEFAULT '',
			target TEXT NOT NULL DEFAULT '',
			node TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			prompt_versions TEXT NOT NULL DEFAULT '{}',
			model TEXT NOT NULL DEFAULT '',
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			disagreed INTEGER NOT NULL DEFAULT 0,
			disagreed_by TEXT NOT NULL DEFAULT '',
			disagreed_note TEXT NOT NULL DEFAULT '',
			disagreed_at INTEGER NOT NULL DEFAULT 0,
			reviewed_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			UNIQUE(run_id, rubric)
		);
		CREATE INDEX IF NOT EXISTS run_grades_run ON run_grades(run_id);
		CREATE INDEX IF NOT EXISTS run_grades_project_created ON run_grades(project, created_at);
		CREATE INDEX IF NOT EXISTS run_grades_fail_unreviewed ON run_grades(pass, blame, disagreed, reviewed_at);

		CREATE TABLE IF NOT EXISTS improvement_proposals (
			id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			target_kind TEXT NOT NULL,
			target TEXT NOT NULL,
			theme TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			before_text TEXT NOT NULL DEFAULT '',
			after_text TEXT NOT NULL DEFAULT '',
			before_hash TEXT NOT NULL DEFAULT '',
			after_hash TEXT NOT NULL DEFAULT '',
			evidence_runs TEXT NOT NULL DEFAULT '[]',
			evidence_grades TEXT NOT NULL DEFAULT '[]',
			model TEXT NOT NULL DEFAULT '',
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			decided_by TEXT NOT NULL DEFAULT '',
			decided_at INTEGER NOT NULL DEFAULT 0,
			decision_note TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS improvement_proposals_status ON improvement_proposals(status, created_at);
		CREATE INDEX IF NOT EXISTS improvement_proposals_target ON improvement_proposals(target_kind, target, status);

		CREATE TABLE IF NOT EXISTS prompt_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			target_kind TEXT NOT NULL,
			target TEXT NOT NULL,
			version_hash TEXT NOT NULL,
			prev_hash TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			proposal_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS prompt_revisions_target ON prompt_revisions(target_kind, target, created_at);
		PRAGMA user_version = 7;`); err != nil {
			return err
		}
	}
	if version < 8 {
		// Rolling LLM usage ledger for 5-hour and weekly quota awareness across
		// both step visits and control-plane calls (planner, grader, map hone).
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS llm_usage (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts INTEGER NOT NULL,
			model TEXT NOT NULL,
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			tokens_cached INTEGER NOT NULL DEFAULT 0,
			calls INTEGER NOT NULL DEFAULT 1,
			source TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS llm_usage_ts ON llm_usage(ts);
		INSERT INTO llm_usage (ts, model, tokens_in, tokens_out, tokens_cached, calls, source)
			SELECT COALESCE(NULLIF(finished_at, 0), NULLIF(started_at, 0), created_at),
				model, tokens_in, tokens_out, tokens_cached,
				CASE WHEN llm_calls > 0 THEN llm_calls ELSE 1 END,
				'backfill'
			FROM visits
			WHERE model != '' AND (tokens_in > 0 OR tokens_out > 0)
			  AND COALESCE(NULLIF(finished_at, 0), NULLIF(started_at, 0), created_at) >= ?;
		PRAGMA user_version = 8;`, now()-14*24*3600*1000); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PinSnapshot is first-writer-wins for legacy runs. Callers reload the run after
// this operation rather than assuming their candidate won.
func (s *Store) PinSnapshot(ctx context.Context, id string, snapshot []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET snapshot = ? WHERE id = ? AND snapshot = ''`, string(snapshot), id)
	return err
}

// Transition atomically updates a visit and its run (gate entry/decision).
func (s *Store) Transition(ctx context.Context, runID string, seq int, visit, run map[string]any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := update(ctx, tx, "visits", "run_id = ? AND seq = ?", []any{runID, seq}, visit, false); err != nil {
		return err
	}
	if err := update(ctx, tx, "runs", "id = ?", []any{runID}, run, false); err != nil {
		return err
	}
	return tx.Commit()
}

// SetRunState commits lifecycle and linked task state together. Cancellation
// also closes every unfinished visit so late results cannot revive work.
func (s *Store) SetRunState(ctx context.Context, r *Run, status, message string, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fields := map[string]any{"status": status, "error": message}
	taskStatus := TaskRunning
	if RunDone(status) {
		fields["finished_at"] = stamp
		taskStatus = TaskFailed
		if status == RunSucceeded {
			taskStatus = TaskSucceeded
		}
	} else {
		fields["started_at"] = stamp
	}
	if err := update(ctx, tx, "runs", "id = ?", []any{r.ID}, fields, false); err != nil {
		return err
	}
	if r.TaskID != "" {
		// Tasks may be deleted independently; the saved flow and run remain.
		if err := update(ctx, tx, "tasks", "id = ?", []any{r.TaskID}, map[string]any{"status": taskStatus, "error": message}, true); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if status == RunCanceled || status == RunFailed {
		visitStatus := VisitCanceled
		if status == RunFailed {
			visitStatus = VisitError
		}
		if _, err := tx.ExecContext(ctx, `UPDATE visits SET status = ?, error = ?, finished_at = ? WHERE run_id = ? AND status IN (?,?,?)`, visitStatus, message, stamp, r.ID, VisitPending, VisitRunning, VisitWaiting); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ErrNotResumable means the run is not in a state a resume can re-open.
var ErrNotResumable = errors.New("only failed or canceled runs can be resumed")

// ResumeRun re-opens a failed or canceled run at node: the run, its task and a
// new pending visit of node commit together, and the engine picks the visit up
// like any other pending work. The status guard makes a repeated resume a no-op.
// Repin moves a resumed run onto another version of its flow.
type Repin struct {
	FlowVersion int
	Snapshot    []byte
}

// ResumeRun reopens a failed or canceled run at node. queued leaves it waiting
// for a run slot; the engine starts it from the pending visit.
func (s *Store) ResumeRun(ctx context.Context, runID, node, typ, note string, repin *Repin, queued bool) (*Visit, error) {
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
	status := RunRunning
	if queued {
		status = RunQueued
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, error = '', finished_at = 0, current_node = ?, resume_seq = ?, resumes = resumes + 1, resume_note = ?
		WHERE id = ? AND status IN (?, ?)`, status, node, seq, note, runID, RunFailed, RunCanceled)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, ErrNotResumable
	}
	if repin != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET flow_version = ?, snapshot = ? WHERE id = ?`, repin.FlowVersion, string(repin.Snapshot), runID); err != nil {
			return nil, err
		}
	}
	var taskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = ?`, runID).Scan(&taskID); err != nil {
		return nil, err
	}
	if taskID != "" {
		if err := update(ctx, tx, "tasks", "id = ?", []any{taskID}, map[string]any{"status": TaskRunning, "error": ""}, true); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	v := &Visit{RunID: runID, Seq: seq + 1, Node: node, Visit: visit + 1, Type: typ, Status: VisitPending, Outputs: json.RawMessage("{}"), CreatedAt: now()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO visits (run_id, seq, node, visit, type, status, outputs, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		v.RunID, v.Seq, v.Node, v.Visit, v.Type, v.Status, "{}", v.CreatedAt); err != nil {
		return nil, err
	}
	return v, tx.Commit()
}

// ProjectSpend aggregates all runs in the half-open UTC month interval; unlike
// ListRuns it has no UI pagination limit. Attribution is by run creation time.
func (s *Store) ProjectSpend(ctx context.Context, project string, start, end int64) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd),0) FROM runs WHERE project = ? AND created_at >= ? AND created_at < ?`, project, start, end).Scan(&total)
	return total, err
}

// ErrFlowActive means a flow still has a queued, running or waiting run.
type ErrFlowActive struct{ Runs []string }

func (e *ErrFlowActive) Error() string {
	return fmt.Sprintf("flow has active runs (%s); cancel or finish them first", strings.Join(e.Runs, ", "))
}

// DeletedFlow reports what DeleteFlow removed.
type DeletedFlow struct {
	Versions int      `json:"versions"`
	Runs     []string `json:"runs"`
}

// DeleteFlow removes every version of a flow with its chat, runs, visits and
// events, and unlinks tasks that pointed at it, in one transaction. It refuses
// with *ErrFlowActive while any run of the flow is queued, running or waiting;
// the check and the deletes share the transaction, so no run can slip between.
func (s *Store) DeleteFlow(ctx context.Context, name string) (*DeletedFlow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &DeletedFlow{Runs: []string{}}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_versions WHERE name = ?`, name).Scan(&out.Versions); err != nil {
		return nil, err
	}
	if out.Versions == 0 {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, status FROM runs WHERE flow_name = ? ORDER BY created_at`, name)
	if err != nil {
		return nil, err
	}
	var active []string
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, err
		}
		out.Runs = append(out.Runs, id)
		if !RunDone(status) {
			active = append(active, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(active) > 0 {
		return nil, &ErrFlowActive{Runs: active}
	}
	for _, q := range []string{
		`DELETE FROM run_grades WHERE run_id IN (SELECT id FROM runs WHERE flow_name = ?)`,
		`DELETE FROM run_facts WHERE run_id IN (SELECT id FROM runs WHERE flow_name = ?)`,
		`DELETE FROM visits WHERE run_id IN (SELECT id FROM runs WHERE flow_name = ?)`,
		`DELETE FROM kv WHERE key LIKE 'notify/stuck/%' AND substr(key, 14, instr(substr(key, 14), '/') - 1) IN (SELECT id FROM runs WHERE flow_name = ?)`,
		`DELETE FROM events WHERE flow_name = ? OR run_id IN (SELECT id FROM runs WHERE flow_name = ?)`,
		`DELETE FROM runs WHERE flow_name = ?`,
		`DELETE FROM chat WHERE flow_name = ?`,
		`DELETE FROM flow_versions WHERE name = ?`,
	} {
		args := []any{name}
		if strings.Count(q, "?") == 2 {
			args = append(args, name)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET flow_name = '', updated_at = ? WHERE flow_name = ?`, now(), name); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
