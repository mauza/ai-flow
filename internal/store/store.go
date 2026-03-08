package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// New opens (or creates) a SQLite database and initializes the schema.
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// SQLite only supports one writer at a time. Limiting to a single
	// connection serializes all access and eliminates SQLITE_BUSY errors
	// from concurrent goroutines.
	db.SetMaxOpenConns(1)

	// Enable WAL mode and set busy timeout
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting pragma %q: %w", pragma, err)
		}
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}

	return &Store{db: db}, nil
}

// RunInfo holds metadata from a previous completed run.
type RunInfo struct {
	ID         int64
	BranchName string
	PRURL      string
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			issue_id    TEXT NOT NULL,
			stage_name  TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'running',
			exit_code   INTEGER,
			output      TEXT,
			pr_url      TEXT,
			branch_name TEXT,
			error       TEXT,
			started_at  DATETIME NOT NULL DEFAULT (datetime('now')),
			ended_at    DATETIME
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_dedup
			ON runs (issue_id, stage_name)
			WHERE status = 'running';

		CREATE TABLE IF NOT EXISTS project_plan_runs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id TEXT NOT NULL,
			stage_name TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'running',
			error      TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_project_plan_runs_active
			ON project_plan_runs(project_id, stage_name)
			WHERE status = 'running';
	`)
	if err != nil {
		return err
	}

	// Migration for existing databases: add branch_name column if missing
	_, _ = db.Exec(`ALTER TABLE runs ADD COLUMN branch_name TEXT`)

	return nil
}

// StartRun attempts to insert a new running record. Returns true if inserted
// (no existing running record), false if a run is already in progress.
func (s *Store) StartRun(issueID, stageName string) (int64, bool, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO runs (issue_id, stage_name, status) VALUES (?, ?, 'running')`,
		issueID, stageName,
	)
	if err != nil {
		return 0, false, fmt.Errorf("inserting run: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("checking rows affected: %w", err)
	}
	if rows == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, true, nil
}

// CompleteRun marks a run as completed with the given exit code, output, optional PR URL, and branch name.
func (s *Store) CompleteRun(runID int64, exitCode int, output, prURL, branchName string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET status = 'completed', exit_code = ?, output = ?, pr_url = ?, branch_name = ?, ended_at = ? WHERE id = ?`,
		exitCode, output, prURL, branchName, time.Now().UTC(), runID,
	)
	return err
}

// FailRun marks a run as failed with the given error message.
func (s *Store) FailRun(runID int64, exitCode int, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET status = 'failed', exit_code = ?, error = ?, ended_at = ? WHERE id = ?`,
		exitCode, errMsg, time.Now().UTC(), runID,
	)
	return err
}

// TimeoutRun marks a run as timed out.
func (s *Store) TimeoutRun(runID int64, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE runs SET status = 'timeout', error = ?, ended_at = ? WHERE id = ?`,
		errMsg, time.Now().UTC(), runID,
	)
	return err
}

// GetLastCompletedRun returns the most recent successful run's branch and PR info for an issue+stage.
// Returns nil if no completed run exists.
func (s *Store) GetLastCompletedRun(issueID, stageName string) (*RunInfo, error) {
	var info RunInfo
	var branchName, prURL sql.NullString
	err := s.db.QueryRow(
		`SELECT id, branch_name, pr_url FROM runs
		 WHERE issue_id = ? AND stage_name = ? AND status = 'completed' AND exit_code = 0
		 ORDER BY ended_at DESC LIMIT 1`,
		issueID, stageName,
	).Scan(&info.ID, &branchName, &prURL)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying last completed run: %w", err)
	}
	info.BranchName = branchName.String
	info.PRURL = prURL.String
	return &info, nil
}

// GetBranchForIssue returns the most recent branch/PR info from ANY completed run for this issue (cross-stage lookup).
// Returns nil if no completed run with a branch exists.
func (s *Store) GetBranchForIssue(issueID string) (*RunInfo, error) {
	var info RunInfo
	var branchName, prURL sql.NullString
	err := s.db.QueryRow(
		`SELECT id, branch_name, pr_url FROM runs
		 WHERE issue_id = ? AND status = 'completed' AND exit_code = 0 AND branch_name IS NOT NULL AND branch_name != ''
		 ORDER BY ended_at DESC LIMIT 1`,
		issueID,
	).Scan(&info.ID, &branchName, &prURL)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying branch for issue: %w", err)
	}
	info.BranchName = branchName.String
	info.PRURL = prURL.String
	return &info, nil
}

// IsRunning checks whether there is currently a running record for the given issue+stage.
func (s *Store) IsRunning(issueID, stageName string) (bool, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM runs WHERE issue_id = ? AND stage_name = ? AND status = 'running'`,
		issueID, stageName,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CleanStaleRuns marks any "running" records older than the given duration as failed.
// This recovers from process crashes that leave zombie running records.
func (s *Store) CleanStaleRuns(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	res, err := s.db.Exec(
		`UPDATE runs SET status = 'failed', error = 'stale run recovered on startup', ended_at = ?
		 WHERE status = 'running' AND started_at < ?`,
		time.Now().UTC(), cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("cleaning stale runs: %w", err)
	}
	return res.RowsAffected()
}

// GetFirstBranchForIssue returns the branch/PR info from the earliest completed run
// that has a branch for this issue. This ensures uses_branch stages always pick up
// the branch created by the first creates_pr stage rather than the most recent run.
func (s *Store) GetFirstBranchForIssue(issueID string) (*RunInfo, error) {
	var info RunInfo
	var branchName, prURL sql.NullString
	err := s.db.QueryRow(
		`SELECT id, branch_name, pr_url FROM runs
		 WHERE issue_id = ? AND status = 'completed' AND exit_code = 0 AND branch_name IS NOT NULL AND branch_name != ''
		 ORDER BY started_at ASC LIMIT 1`,
		issueID,
	).Scan(&info.ID, &branchName, &prURL)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying first branch for issue: %w", err)
	}
	info.BranchName = branchName.String
	info.PRURL = prURL.String
	return &info, nil
}

// RunRecord holds the full data for a single pipeline run.
type RunRecord struct {
	ID         int64      `json:"id"`
	IssueID    string     `json:"issue_id"`
	StageName  string     `json:"stage_name"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exit_code"`
	Output     string     `json:"output"`
	PRURL      string     `json:"pr_url"`
	BranchName string     `json:"branch_name"`
	Error      string     `json:"error"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
}

// ListRecentRuns returns the most recent runs, newest first.
func (s *Store) ListRecentRuns(limit int) ([]RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, issue_id, stage_name, status, exit_code,
		        COALESCE(output,''), COALESCE(pr_url,''), COALESCE(branch_name,''),
		        COALESCE(error,''), started_at, ended_at
		 FROM runs ORDER BY started_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying recent runs: %w", err)
	}
	defer rows.Close()

	var records []RunRecord
	for rows.Next() {
		r, err := scanRunRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// GetRun returns a single run by ID.
func (s *Store) GetRun(id int64) (*RunRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, issue_id, stage_name, status, exit_code,
		        COALESCE(output,''), COALESCE(pr_url,''), COALESCE(branch_name,''),
		        COALESCE(error,''), started_at, ended_at
		 FROM runs WHERE id = ?`,
		id,
	)
	r, err := scanRunRecord(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying run %d: %w", id, err)
	}
	return &r, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRecord(row rowScanner) (RunRecord, error) {
	var r RunRecord
	var exitCode sql.NullInt64
	var endedAt sql.NullTime
	err := row.Scan(
		&r.ID, &r.IssueID, &r.StageName, &r.Status,
		&exitCode, &r.Output, &r.PRURL, &r.BranchName,
		&r.Error, &r.StartedAt, &endedAt,
	)
	if err != nil {
		return r, err
	}
	if exitCode.Valid {
		ec := int(exitCode.Int64)
		r.ExitCode = &ec
	}
	if endedAt.Valid {
		r.EndedAt = &endedAt.Time
	}
	return r, nil
}

// StartProjectRun inserts a new running record for a project stage.
// Returns the run ID, or an error (including a unique constraint error if already running).
func (s *Store) StartProjectRun(projectID, stageName string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO project_plan_runs (project_id, stage_name, status) VALUES (?, ?, 'running')`,
		projectID, stageName,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting project run: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("checking rows affected: %w", err)
	}
	if rows == 0 {
		return 0, fmt.Errorf("project run already in progress for project %s stage %s", projectID, stageName)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	return id, nil
}

// CompleteProjectRun marks a project plan run as completed.
func (s *Store) CompleteProjectRun(id int64) error {
	_, err := s.db.Exec(
		`UPDATE project_plan_runs SET status = 'completed', updated_at = ? WHERE id = ?`,
		time.Now().UTC(), id,
	)
	return err
}

// FailProjectRun marks a project plan run as failed with an error message.
func (s *Store) FailProjectRun(id int64, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE project_plan_runs SET status = 'failed', error = ?, updated_at = ? WHERE id = ?`,
		errMsg, time.Now().UTC(), id,
	)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
