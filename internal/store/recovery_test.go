package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrateOriginalDatabaseAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	check(t, err)
	_, err = db.Exec(schema)
	check(t, err)
	_, err = db.Exec(`INSERT INTO runs (id, flow_name, flow_version, status, created_at, cost_usd) VALUES ('old','f',1,'running',123,2.5);
	INSERT INTO visits (run_id,seq,node,visit,type,status,created_at) VALUES ('old',1,'gate',1,'gate','pending',123)`)
	check(t, err)
	check(t, db.Close())
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		check(t, err)
		r, err := s.GetRun(ctx, "old")
		check(t, err)
		if r.CostUSD != 2.5 || len(r.Snapshot) != 0 {
			t.Fatalf("old row changed: %+v", r)
		}
		v, err := s.LastVisit(ctx, "old")
		check(t, err)
		if v.Status != VisitPending || v.LaunchState != "" {
			t.Fatalf("old visit changed: %+v", v)
		}
		var version int
		check(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
		if version != 3 {
			t.Fatalf("version %d", version)
		}
		check(t, s.Ping(ctx))
		check(t, s.Close())
		if s.Ping(ctx) == nil {
			t.Fatal("closed database is ready")
		}
	}
}

func TestGateTransitionRollsBackBothRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := &Run{ID: "r", FlowName: "f", Status: RunRunning}
	check(t, s.CreateRun(ctx, r))
	v, err := s.AddVisit(ctx, r.ID, "gate", "gate")
	check(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER fail_gate BEFORE UPDATE ON runs WHEN NEW.status = 'waiting' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	check(t, err)
	err = s.Transition(ctx, r.ID, v.Seq, map[string]any{"status": VisitWaiting}, map[string]any{"status": RunWaiting})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	v, err = s.GetVisit(ctx, r.ID, v.Seq)
	check(t, err)
	r, err = s.GetRun(ctx, r.ID)
	check(t, err)
	if v.Status != VisitPending || r.Status != RunRunning {
		t.Fatalf("partial transition: %s/%s", v.Status, r.Status)
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_gate`)
	check(t, err)
	check(t, s.Transition(ctx, r.ID, v.Seq, map[string]any{"status": VisitWaiting}, map[string]any{"status": RunWaiting}))
}

func TestLifecycleAndVisitPointerTransactions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.AddVisit(ctx, "missing", "gate", "gate"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	vs, err := s.Visits(ctx, "missing")
	check(t, err)
	if len(vs) != 0 {
		t.Fatal("orphan visit committed")
	}
	r := &Run{ID: "r", FlowName: "f", Status: RunRunning, TaskID: "missing-task"}
	check(t, s.CreateRun(ctx, r))
	check(t, s.CreateTask(ctx, &Task{ID: r.TaskID, Source: "test", Title: "task"}))
	_, err = s.db.Exec(`CREATE TRIGGER fail_task BEFORE UPDATE ON tasks BEGIN SELECT RAISE(ABORT, 'injected task failure'); END`)
	check(t, err)
	if err := s.SetRunState(ctx, r, RunSucceeded, "", Now()); err == nil {
		t.Fatal("actual task write error was ignored")
	}
	fresh, err := s.GetRun(ctx, r.ID)
	check(t, err)
	if fresh.Status != RunRunning {
		t.Fatal("partial lifecycle commit")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_task`)
	check(t, err)
	v, err := s.AddVisit(ctx, r.ID, "gate", "gate")
	check(t, err)
	check(t, s.SetRunState(ctx, r, RunCanceled, "canceled", Now()))
	v, err = s.GetVisit(ctx, r.ID, v.Seq)
	check(t, err)
	task, err := s.GetTask(ctx, r.TaskID)
	check(t, err)
	if v.Status != VisitCanceled || task.Status != TaskFailed {
		t.Fatalf("cancel: visit=%s task=%s", v.Status, task.Status)
	}
}

func TestLifecycleAllowsDeletedTask(t *testing.T) {
	for _, status := range []string{RunRunning, RunSucceeded, RunFailed, RunCanceled} {
		t.Run(status, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			r := &Run{ID: "r", FlowName: "f", Status: RunQueued, TaskID: "task"}
			check(t, s.CreateTask(ctx, &Task{ID: "task", Source: "test", Title: "task"}))
			check(t, s.CreateRun(ctx, r))
			v, err := s.AddVisit(ctx, r.ID, "step", "action")
			check(t, err)
			check(t, s.DeleteTask(ctx, r.TaskID))
			check(t, s.SetRunState(ctx, r, status, "reason", Now()))
			fresh, err := s.GetRun(ctx, r.ID)
			check(t, err)
			if fresh.Status != status {
				t.Fatalf("status %s", fresh.Status)
			}
			if status == RunCanceled || status == RunFailed {
				v, err = s.GetVisit(ctx, r.ID, v.Seq)
				check(t, err)
				want := VisitCanceled
				if status == RunFailed {
					want = VisitError
				}
				if v.Status != want {
					t.Fatalf("unfinished visit %s", v.Status)
				}
			}
		})
	}
}

func TestProjectSpendIsUnpaginatedAndMonthBounded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	tx, err := s.db.BeginTx(ctx, nil)
	check(t, err)
	for i := 0; i < 2105; i++ {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (id,flow_name,flow_version,status,project,cost_usd,created_at) VALUES (?,'f',1,'succeeded','p',1,?)`, fmt.Sprint(i), start.UnixMilli()+int64(i))
		check(t, err)
	}
	for i, row := range []struct {
		project string
		at      int64
	}{{"other", start.UnixMilli()}, {"p", start.UnixMilli() - 1}, {"p", end.UnixMilli()}} {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (id,flow_name,flow_version,status,project,cost_usd,created_at) VALUES (?,'f',1,'succeeded',?,999,?)`, fmt.Sprintf("excluded%d", i), row.project, row.at)
		check(t, err)
	}
	check(t, tx.Commit())
	total, err := s.ProjectSpend(ctx, "p", start.UnixMilli(), end.UnixMilli())
	check(t, err)
	if total != 2105 {
		t.Fatalf("total %v", total)
	}
	total, err = s.ProjectSpend(ctx, "empty", start.UnixMilli(), end.UnixMilli())
	check(t, err)
	if total != 0 {
		t.Fatalf("empty total %v", total)
	}
}

func TestUsageMissingVisitDoesNotChargeRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	check(t, s.CreateRun(ctx, &Run{ID: "r", FlowName: "f", Status: RunRunning}))
	if err := s.AddUsage(ctx, "r", 99, "model", 10, 20, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	r, err := s.GetRun(ctx, "r")
	check(t, err)
	if r.CostUSD != 0 || r.Tokens != 0 {
		t.Fatalf("partial usage: %+v", r)
	}
}
