package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestOperationsExactGroupsAndUnpaginatedQueue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	oldest := Now() - 90_000
	tx, err := s.db.BeginTx(ctx, nil)
	check(t, err)
	defer tx.Rollback()
	for i := 0; i < 253; i++ {
		status := RunQueued
		if i == 251 || i == 252 {
			status = RunRunning
		}
		if i == 250 {
			status = RunWaiting
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (id,flow_name,flow_version,status,created_at,snapshot) VALUES (?,'queue',1,?,?,'not needed for operations')`, fmt.Sprint(i), status, oldest+int64(i))
		check(t, err)
	}
	for version := 1; version <= 2; version++ {
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (id,flow_name,flow_version,project,status,created_at) VALUES (?,'flow',?,'p','succeeded',1)`, fmt.Sprintf("r%d", version), version)
		check(t, err)
	}
	visits := []struct {
		status, outcome, failure string
		start, finish            int64
	}{
		{VisitSucceeded, "done", "", 100, 125},
		{VisitSucceeded, "fail", "", 200, 240},
		{VisitSucceeded, "timeout", "", 500, 500},
		{VisitError, "", "node exceeded its timeout", 700, 730},
		{VisitError, "", "provider failed", 1000, 1050},
		{VisitCanceled, "timeout", "node exceeded its timeout", 0, 1200},
		{VisitRunning, "fail", "", 200, 0},
		{VisitSucceeded, "done", "", 0, 0},
		{VisitError, "", "other error", 900, 800},
	}
	for i, v := range visits {
		_, err := tx.ExecContext(ctx, `INSERT INTO visits (run_id,seq,node,visit,type,status,outcome,error,started_at,finished_at,created_at) VALUES (?,?,'step',1,'check',?,?,?,?,?,1)`, fmt.Sprintf("r%d", i%2+1), i+1, v.status, v.outcome, v.failure, v.start, v.finish)
		check(t, err)
	}
	// Additional keys prove grouping/sorting by project, flow, node, and type.
	keys := [][4]string{{"p", "flow", "step", "agent"}, {"p", "flow", "alpha", "check"}, {"p", "aaa", "step", "check"}, {"a", "flow", "step", "check"}}
	for i, key := range keys {
		id := fmt.Sprintf("extra%d", i)
		_, err := tx.ExecContext(ctx, `INSERT INTO runs (id,flow_name,flow_version,project,status,created_at) VALUES (?,?,1,?,'succeeded',1)`, id, key[1], key[0])
		check(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO visits (run_id,seq,node,visit,type,status,created_at) VALUES (?,1,?,1,?,'succeeded',1)`, id, key[2], key[3])
		check(t, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO visits (run_id,seq,node,visit,type,status,created_at) VALUES ('orphan',1,'step',1,'check','error',1)`)
	check(t, err)
	check(t, tx.Commit())
	out, err := s.Operations(ctx)
	check(t, err)
	if out.Active != 2 || out.Waiting != 1 || out.Queued != 250 || out.OldestQueuedAgeMS != out.AsOf-oldest {
		t.Fatalf("queue: %+v", out)
	}
	if len(out.Nodes) != 5 {
		t.Fatalf("groups: %+v", out.Nodes)
	}
	wantKeys := [][4]string{{"a", "flow", "step", "check"}, {"p", "aaa", "step", "check"}, {"p", "flow", "alpha", "check"}, {"p", "flow", "step", "agent"}, {"p", "flow", "step", "check"}}
	for i, n := range out.Nodes {
		if got := [4]string{n.Project, n.FlowName, n.Node, n.Type}; got != wantKeys[i] {
			t.Fatalf("group %d: %v", i, got)
		}
		if i < 4 && (n.Visits != 1 || n.DurationSamples != 0 || len(n.Failures) != 0) {
			t.Fatalf("extra group: %+v", n)
		}
	}
	n := out.Nodes[4]
	if n.Visits != 9 || n.DurationSamples != 5 || n.DurationTotalMS != 145 || n.DurationMaxMS != 50 {
		t.Fatalf("durations: %+v", n)
	}
	if !reflect.DeepEqual(n.Failures, map[string]int{"canceled": 1, "error": 2, "timeout": 2, "fail_outcome": 1}) {
		t.Fatalf("failures: %v", n.Failures)
	}
}

func TestOperationsEmptyFutureAndErrors(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	out, err := s.Operations(ctx)
	check(t, err)
	if out.Nodes == nil || len(out.Nodes) != 0 || out.Queued != 0 || out.OldestQueuedAgeMS != 0 {
		t.Fatalf("empty: %+v", out)
	}
	_, err = s.db.Exec(`INSERT INTO runs (id,flow_name,flow_version,status,created_at) VALUES ('future','f',1,'queued',?)`, Now()+100_000)
	check(t, err)
	out, err = s.Operations(ctx)
	check(t, err)
	if out.Queued != 1 || out.OldestQueuedAgeMS != 0 {
		t.Fatalf("future: %+v", out)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if out, err := s.Operations(canceled); err == nil || out != nil {
		t.Fatalf("canceled returned partial view: %v, %v", out, err)
	}
	_, err = s.db.Exec(`DROP TABLE visits`)
	check(t, err)
	if out, err := s.Operations(ctx); err == nil || out != nil {
		t.Fatalf("failed visit aggregate returned partial view: %v, %v", out, err)
	}
	check(t, s.Ping(ctx)) // the failed read transaction released the connection
	check(t, s.Close())
	if out, err := s.Operations(ctx); err == nil || out != nil {
		t.Fatalf("closed store: %v, %v", out, err)
	}
}
