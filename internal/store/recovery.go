package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	if version > 3 {
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
func (s *Store) ResumeRun(ctx context.Context, runID, node, typ, note string) (*Visit, error) {
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
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, error = '', finished_at = 0, current_node = ?, resume_seq = ?, resumes = resumes + 1, resume_note = ?
		WHERE id = ? AND status IN (?, ?)`, RunRunning, node, seq, note, runID, RunFailed, RunCanceled)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, ErrNotResumable
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
