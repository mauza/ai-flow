package store

import (
	"context"
	"errors"
	"testing"
)

func TestDeleteFlowRemovesEverythingButOtherFlows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, n := range []string{"doomed", "keep"} {
		for i := 0; i < 2; i++ {
			_, err := s.SaveFlow(ctx, &FlowVersion{Name: n, YAML: "y"})
			check(t, err)
		}
		check(t, s.AddChat(ctx, &ChatMessage{FlowName: n, Role: "user", Content: "hi"}))
		check(t, s.CreateRun(ctx, &Run{ID: "r-" + n, FlowName: n, FlowVersion: 1, Status: RunFailed}))
		_, err := s.AddVisit(ctx, "r-"+n, "a", "check")
		check(t, err)
		check(t, s.AddEvent(ctx, &Event{RunID: "r-" + n, FlowName: n, Kind: "x", Message: "m"}))
		check(t, s.SetKV(ctx, "notify/stuck/r-"+n+"/1", "1"))
		check(t, s.CreateTask(ctx, &Task{ID: "t-" + n, Source: "ui", Title: n, FlowName: n}))
	}
	check(t, s.UpdateRun(ctx, "r-doomed", map[string]any{"status": RunFailed}))
	check(t, s.UpdateRun(ctx, "r-keep", map[string]any{"status": RunRunning})) // active, but another flow

	res, err := s.DeleteFlow(ctx, "doomed")
	check(t, err)
	if res.Versions != 2 || len(res.Runs) != 1 || res.Runs[0] != "r-doomed" {
		t.Fatalf("result %+v", res)
	}
	if _, err := s.GetFlow(ctx, "doomed", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("flow still there: %v", err)
	}
	if _, err := s.GetRun(ctx, "r-doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("run still there: %v", err)
	}
	if vs, _ := s.Visits(ctx, "r-doomed"); len(vs) != 0 {
		t.Errorf("visits still there: %d", len(vs))
	}
	if es, _ := s.Events(ctx, "flow_name", "doomed", 10); len(es) != 0 {
		t.Errorf("events still there: %d", len(es))
	}
	if cs, _ := s.Chat(ctx, "doomed"); len(cs) != 0 {
		t.Errorf("chat still there: %d", len(cs))
	}
	if _, err := s.GetKV(ctx, "notify/stuck/r-doomed/1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stuck marker still there: %v", err)
	}
	if task, _ := s.GetTask(ctx, "t-doomed"); task == nil || task.FlowName != "" {
		t.Errorf("task should stay, unlinked: %+v", task)
	}

	// The other flow is untouched.
	if v, _ := s.FlowVersions(ctx, "keep"); len(v) != 2 {
		t.Errorf("keep versions %d", len(v))
	}
	if vs, _ := s.Visits(ctx, "r-keep"); len(vs) != 1 {
		t.Errorf("keep visits %d", len(vs))
	}
	if _, err := s.GetKV(ctx, "notify/stuck/r-keep/1"); err != nil {
		t.Errorf("keep marker: %v", err)
	}
	if task, _ := s.GetTask(ctx, "t-keep"); task.FlowName != "keep" {
		t.Errorf("keep task %+v", task)
	}

	if _, err := s.DeleteFlow(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestDeleteFlowRefusesWhileARunIsActive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, err := s.SaveFlow(ctx, &FlowVersion{Name: "busy", YAML: "y"})
	check(t, err)
	check(t, s.CreateRun(ctx, &Run{ID: "r-old", FlowName: "busy", FlowVersion: 1, Status: RunSucceeded}))
	for _, st := range []string{RunQueued, RunRunning, RunWaiting} {
		check(t, s.CreateRun(ctx, &Run{ID: "r-" + st, FlowName: "busy", FlowVersion: 1, Status: st}))
		_, err := s.DeleteFlow(ctx, "busy")
		var active *ErrFlowActive
		if !errors.As(err, &active) || active.Runs[len(active.Runs)-1] != "r-"+st {
			t.Fatalf("%s: %v", st, err)
		}
		if _, err := s.GetRun(ctx, "r-old"); err != nil {
			t.Fatalf("a refused delete removed something: %v", err)
		}
		check(t, s.UpdateRun(ctx, "r-"+st, map[string]any{"status": RunCanceled}))
	}
	if _, err := s.DeleteFlow(ctx, "busy"); err != nil {
		t.Fatalf("all runs finished: %v", err)
	}
}
