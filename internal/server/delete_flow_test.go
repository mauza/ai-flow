package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/store"
)

func TestDeleteFlowEndpoint(t *testing.T) {
	base := testServer(t, "")
	a := base.app
	a.Engine = engine.New(a.Cfg, a.Store, nil, a.Hub, nil)
	obj, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(a, obj, fstest.MapFS{"index.html": {Data: []byte("ui")}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	ctx := context.Background()
	if _, err := a.Store.SaveFlow(ctx, &store.FlowVersion{Name: "smoke", YAML: "y"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*store.Run{{ID: "r-done", FlowName: "smoke", FlowVersion: 1, Status: store.RunSucceeded}, {ID: "r-live", FlowName: "smoke", FlowVersion: 1, Status: store.RunWaiting}} {
		if err := a.Store.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	obj.Put(ctx, "runs/r-done/001-a.final.jsonl", []byte("t"), "")
	obj.Put(ctx, "runs/r-donex/001-a.final.jsonl", []byte("other run, similar id"), "")

	del := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/flows/smoke", nil))
		return w
	}
	if w := del(); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "r-live") {
		t.Fatalf("active run: %d %s", w.Code, w.Body)
	}
	if _, err := a.Store.GetFlow(ctx, "smoke", 0); err != nil {
		t.Fatalf("a refused delete removed the flow: %v", err)
	}

	a.Store.UpdateRun(ctx, "r-live", map[string]any{"status": store.RunCanceled})
	w := del()
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	var res store.DeletedFlow
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Versions != 1 || len(res.Runs) != 2 {
		t.Errorf("result %+v", res)
	}
	if _, err := obj.Get(ctx, "runs/r-done/001-a.final.jsonl"); err != objstore.ErrNotFound {
		t.Errorf("transcript survived: %v", err)
	}
	if _, err := obj.Get(ctx, "runs/r-donex/001-a.final.jsonl"); err != nil {
		t.Errorf("another run's transcript was removed: %v", err)
	}
	if w := del(); w.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", w.Code)
	}
}
