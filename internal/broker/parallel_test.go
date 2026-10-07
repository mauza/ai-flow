package broker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/store"
)

func TestBranchVisitsAreCurrentWhileTheirParallelVisitRuns(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "par.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateRun(ctx, &store.Run{ID: "r", Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	fork, _ := st.AddVisit(ctx, "r", "checks", flow.TypeParallel)
	st.UpdateVisit(ctx, "r", fork.Seq, map[string]any{"status": store.VisitRunning})
	lint, _ := st.AddLaneVisit(ctx, "r", "lint", flow.TypeCheck, fork.Seq, "lint")
	test, _ := st.AddLaneVisit(ctx, "r", "test", flow.TypeCheck, fork.Seq, "test")
	b := New(&config.Config{}, st, nil, grant.New([]byte("k")), &runtimeObjects{}, hub.New(), nil, nil)
	check := func(v *store.Visit) error {
		return b.checkGrantVisit(ctx, &grant.Claims{Run: "r", Seq: v.Seq, Node: v.Node}, false)
	}

	if err := check(lint); err != nil {
		t.Errorf("lint branch: %v", err)
	}
	if err := check(test); err != nil {
		t.Errorf("test branch: %v", err)
	}
	// A newer visit in the same branch supersedes the older one.
	st.UpdateVisit(ctx, "r", test.Seq, map[string]any{"status": store.VisitSucceeded})
	diag, _ := st.AddLaneVisit(ctx, "r", "diagnose", flow.TypeAgent, fork.Seq, "test")
	if err := check(diag); err != nil {
		t.Errorf("diagnose: %v", err)
	}
	if r, _ := st.GetRun(ctx, "r"); r.CurrentNode != "checks" {
		t.Errorf("branch visits must not move the run pointer: %q", r.CurrentNode)
	}
	// Once the parallel visit is done, branch grants stop working.
	st.UpdateVisit(ctx, "r", fork.Seq, map[string]any{"status": store.VisitSucceeded})
	if err := check(lint); err == nil {
		t.Error("a branch outlived its parallel visit")
	}
}
