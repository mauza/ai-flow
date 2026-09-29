package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

func TestOptionalTaskDoesNotHideDatabaseErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	e := &Engine{store: st}
	r := &store.Run{ID: "run", TaskID: "deleted"}
	ctx := context.Background()
	if task, err := e.optionalTask(ctx, r); err != nil || task != nil {
		t.Fatalf("optional task: %v, %v", task, err)
	}
	res := &resolve.Resolved{Flow: &flow.Flow{}}
	if _, err := e.checkedTemplateContext(ctx, r, res, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.optionalTask(ctx, r); err == nil || permanent(err) {
		t.Fatalf("database error hidden or made permanent: %v", err)
	}
	if _, err := e.checkedTemplateContext(ctx, r, res, nil); err == nil || permanent(err) {
		t.Fatalf("context database error hidden or made permanent: %v", err)
	}
}

func TestErrorRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		err       error
		permanent bool
	}{
		{rejectf("invalid settings"), true},
		{errors.New("database is locked"), false},
		{context.DeadlineExceeded, false},
		{&github.APIError{Status: 401}, true},
		{&github.APIError{Status: 403}, true},
		{&github.APIError{Status: 403, Body: "API rate limit exceeded"}, false},
		{&github.APIError{Status: 408}, false},
		{&github.APIError{Status: 422}, true},
		{&github.APIError{Status: 429}, false},
		{&github.APIError{Status: 500}, false},
	} {
		if got := permanent(tc.err); got != tc.permanent {
			t.Errorf("%v: permanent=%v", tc.err, got)
		}
	}
}
