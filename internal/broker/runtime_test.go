package broker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/protocol"
	"github.com/mauza/ai-flow/internal/store"
)

type runtimeObjects struct{ puts, size int }

func (o *runtimeObjects) Put(_ context.Context, _ string, data []byte, _ string) error {
	o.puts++
	o.size = len(data)
	return nil
}
func (*runtimeObjects) Get(context.Context, string) ([]byte, error) { return nil, nil }

func runtimeBroker(t *testing.T) (*Broker, *grant.Claims, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateRun(ctx, &store.Run{ID: "runtime", Status: store.RunRunning, CurrentNode: "check"}); err != nil {
		t.Fatal(err)
	}
	v, err := st.AddVisit(ctx, "runtime", "check", flow.TypeCheck)
	if err != nil {
		t.Fatal(err)
	}
	signer := grant.New([]byte("test-only-runtime-signing-key"))
	c := &grant.Claims{Kind: "grant", Run: v.RunID, Seq: v.Seq, Node: v.Node, Repo: "example.invalid/o/r", Branch: "b", Write: true}
	token, err := signer.Mint(*c, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b := New(&config.Config{}, st, nil, signer, &runtimeObjects{}, hub.New(), nil, nil)
	return b, c, token
}

func runtimeRequest(h http.Handler, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRuntimeGrantLifecycle(t *testing.T) {
	for _, state := range []string{"pending", "running", "succeeded", "error", "canceled", "waiting", "run-canceled", "run-succeeded", "run-failed", "run-waiting", "run-queued", "stale", "wrong-node", "deadline", "wrong-type"} {
		t.Run(state, func(t *testing.T) {
			b, c, token := runtimeBroker(t)
			ctx := context.Background()
			var err error
			switch {
			case strings.HasPrefix(state, "run-"):
				err = b.store.UpdateRun(ctx, c.Run, map[string]any{"status": strings.TrimPrefix(state, "run-")})
			case state == "stale":
				_, err = b.store.AddVisit(ctx, c.Run, c.Node, flow.TypeCheck)
			case state == "wrong-node":
				err = b.store.UpdateRun(ctx, c.Run, map[string]any{"current_node": "other"})
			case state == "deadline":
				err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"deadline": store.Now() - 1})
			case state == "wrong-type":
				err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"type": flow.TypeGate})
			default:
				err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"status": state})
			}
			if err != nil {
				t.Fatal(err)
			}
			active := state == "pending" || state == "running"
			called := false
			h := b.withGrant(func(w http.ResponseWriter, _ *http.Request, _ *grant.Claims) { called = true; w.WriteHeader(204) })
			w := runtimeRequest(h, "POST", "/", token, nil)
			if called != active || (w.Code == 204) != active {
				t.Fatalf("authorization: called=%v status=%d", called, w.Code)
			}
			if !active {
				// Actual route wiring, including Git's independent Basic auth path.
				for _, route := range []string{"/v1/progress", "/v1/mcp/call", "/llm/v1/chat/completions", "/llm/v1/models", "/git/example.invalid/o/r.git/git-upload-pack", "/git/example.invalid/o/r.git/git-receive-pack"} {
					method := "POST"
					if route == "/llm/v1/models" {
						method = "GET"
					}
					r := httptest.NewRequest(method, route, strings.NewReader("{}"))
					r.Header.Set("Authorization", "Bearer "+token)
					if strings.HasPrefix(route, "/git/") {
						r.SetBasicAuth("node", token)
					}
					w := httptest.NewRecorder()
					b.Handler().ServeHTTP(w, r)
					if w.Code != 403 {
						t.Errorf("%s: status %d", route, w.Code)
					}
				}
				if _, err := b.buildBundle(ctx, c.Run, c.Seq); err == nil {
					t.Error("exchange accepted inactive visit")
				}
			}
		})
	}
}

func TestRuntimeGrantIdentityAndDelivery(t *testing.T) {
	b, c, token := runtimeBroker(t)
	ctx := context.Background()
	for _, status := range []string{store.VisitSucceeded, store.VisitError, store.VisitCanceled} {
		if err := b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"status": status, "summary": "original"}); err != nil {
			t.Fatal(err)
		}
		if err := b.store.UpdateRun(ctx, c.Run, map[string]any{"status": store.RunCanceled}); err != nil {
			t.Fatal(err)
		}
		w := runtimeRequest(b.Handler(), "POST", "/v1/result", token, strings.NewReader(`{"outcome":"forged","summary":"overwrite"}`))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"accepted":false`) {
			t.Fatalf("idempotent result: %d %s", w.Code, w.Body)
		}
		v, err := b.store.GetVisit(ctx, c.Run, c.Seq)
		if err != nil || v.Summary != "original" || v.Status != status {
			t.Fatalf("result mutated visit: %+v %v", v, err)
		}
		w = runtimeRequest(b.Handler(), "POST", "/v1/transcript", token, strings.NewReader("final log"))
		if w.Code != 204 {
			t.Fatalf("final transcript: %d %s", w.Code, w.Body)
		}
	}
	for _, bad := range []grant.Claims{
		{Kind: "grant", Run: c.Run, Seq: c.Seq, Node: "other"},
		{Kind: "grant", Run: c.Run, Seq: 99, Node: c.Node},
		{Kind: "grant", Run: "missing", Seq: c.Seq, Node: c.Node},
	} {
		tok, err := b.signer.Mint(bad, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, route := range []string{"/v1/result", "/v1/transcript"} {
			if w := runtimeRequest(b.Handler(), "POST", route, tok, strings.NewReader("{}")); w.Code != 403 {
				t.Errorf("identity accepted: %s %d", route, w.Code)
			}
		}
	}
}

func TestDeliveryDuringReconciliationGrace(t *testing.T) {
	for _, status := range []string{store.VisitPending, store.VisitRunning} {
		for _, age := range []time.Duration{30 * time.Second, 61 * time.Second} {
			t.Run(status+"/"+age.String(), func(t *testing.T) {
				b, c, token := runtimeBroker(t)
				b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
				ctx := context.Background()
				if err := b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"status": status, "deadline": time.Now().Add(-age).UnixMilli()}); err != nil {
					t.Fatal(err)
				}
				// The execution grant is expired throughout the delivery grace.
				if err := b.checkGrantVisit(ctx, c, false); err == nil {
					t.Fatal("expired execution authorized")
				}
				w := runtimeRequest(b.Handler(), "POST", "/v1/transcript", token, strings.NewReader("late logs"))
				want := http.StatusNoContent
				if age > time.Minute {
					want = http.StatusForbidden
				}
				if w.Code != want {
					t.Fatalf("transcript status=%d want=%d: %s", w.Code, want, w.Body)
				}
				w = runtimeRequest(b.Handler(), "POST", "/v1/result", token, strings.NewReader(`{"outcome":"pass","summary":"delivered during grace"}`))
				if age > time.Minute {
					if w.Code != http.StatusForbidden {
						t.Fatalf("result after grace: %d", w.Code)
					}
					return
				}
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
					t.Fatalf("result during grace: %d %s", w.Code, w.Body)
				}
				w = runtimeRequest(b.Handler(), "POST", "/v1/result", token, strings.NewReader(`{"outcome":"fail"}`))
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":false`) {
					t.Fatalf("duplicate result: %d %s", w.Code, w.Body)
				}
			})
		}
	}
}

// Like the supported local object store, a Put already underway ignores request
// cancellation. The gate reproduces a client timing out before the disk write.
type delayedTranscriptStore struct {
	objstore.Store
	started chan struct{}
	release chan struct{}
	puts    atomic.Int32
}

func (s *delayedTranscriptStore) Put(ctx context.Context, key string, data []byte, typ string) error {
	s.puts.Add(1)
	if string(data) == "periodic" {
		close(s.started)
		<-s.release
	}
	return s.Store.Put(ctx, key, data, typ)
}

func TestFinalTranscriptWaitsForServerWriteAndResultSealsDelivery(t *testing.T) {
	b, c, token := runtimeBroker(t)
	b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
	local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obj := &delayedTranscriptStore{Store: local, started: make(chan struct{}), release: make(chan struct{})}
	b.obj = obj
	var once sync.Once
	release := func() { once.Do(func() { close(obj.release) }) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader("periodic")).WithContext(ctx)
	old.Header.Set("Authorization", "Bearer "+token)
	oldDone := make(chan struct{})
	go func() { defer close(oldDone); b.Handler().ServeHTTP(httptest.NewRecorder(), old) }()
	select {
	case <-obj.started:
	case <-time.After(time.Second):
		t.Fatal("periodic write did not start")
	}
	cancel() // Client timeout does not finish the old Put.
	final := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader("final"))
	final.Header.Set("Authorization", "Bearer "+token)
	final.Header.Set(protocol.TranscriptFinalHeader, "true")
	w := httptest.NewRecorder()
	finalDone := make(chan struct{})
	go func() { defer close(finalDone); b.Handler().ServeHTTP(w, final) }()
	select {
	case <-finalDone:
		t.Error("final request finished while old server write was still running")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	for _, done := range []chan struct{}{oldDone, finalDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("transcript handler did not finish")
		}
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("final status=%d: %s", w.Code, w.Body)
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	data, err := local.Get(context.Background(), v.TranscriptKey)
	if err != nil || string(data) != "final" {
		t.Fatalf("final overwritten by canceled request: %q, %v", data, err)
	}

	// Acceptance, not final publication alone, freezes the chosen artifact.
	w = attemptResult(t, b, token, v.TranscriptKey, "accepted")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("result: %d %s", w.Code, w.Body)
	}
	restarted := New(b.cfg, b.store, b.engine, b.signer, local, b.hub, nil, nil)
	for _, finalFlag := range []string{"", "true"} {
		req := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader("late replacement"))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(protocol.TranscriptFinalHeader, finalFlag)
		w := httptest.NewRecorder()
		restarted.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("idempotent delivery: %d %s", w.Code, w.Body)
		}
	}
	v, err = b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	data, err = local.Get(context.Background(), v.TranscriptKey)
	if err != nil || string(data) != "final" {
		t.Fatalf("terminal grant rewrote final: %q, %v", data, err)
	}
	b.transcriptMu.Lock()
	defer b.transcriptMu.Unlock()
	if len(b.transcriptWrites) != 0 {
		t.Fatalf("leaked visit locks: %d", len(b.transcriptWrites))
	}
}

func TestTranscriptLockWaitCancellationAndCleanup(t *testing.T) {
	b, _, _ := runtimeBroker(t)
	unlock, err := b.lockTranscript(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.lockTranscript(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	other() // Unrelated visits do not wait on this stalled write.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if release, err := b.lockTranscript(ctx, "one"); err == nil {
		release()
		t.Error("canceled waiter acquired lock")
	}
	unlock()
	b.transcriptMu.Lock()
	defer b.transcriptMu.Unlock()
	if len(b.transcriptWrites) != 0 {
		t.Fatalf("leaked visit locks: %d", len(b.transcriptWrites))
	}
}

func uploadAttemptTranscript(t *testing.T, b *Broker, c *grant.Claims, token, data string) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set(protocol.TranscriptFinalHeader, "true")
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("final upload: %d %s", w.Code, w.Body)
	}
	return fmt.Sprintf("runs/%s/%03d-%s.final-%x.jsonl", c.Run, c.Seq, c.Node, sha256.Sum256([]byte(data)))
}

func attemptResult(t *testing.T, b *Broker, token, key, summary string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"outcome": "pass", "summary": summary, "transcript_key": key})
	if err != nil {
		t.Fatal(err)
	}
	return runtimeRequest(b.Handler(), "POST", "/v1/result", token, strings.NewReader(string(raw)))
}

func TestRetriedVisitBindsTranscriptToAcceptedResult(t *testing.T) {
	b, c, token := runtimeBroker(t)
	b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
	local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.obj = local
	first := uploadAttemptTranscript(t, b, c, token, "attempt one")
	// No result: the Job can restart with the same visit-scoped capability.
	// Restart the broker too, so no in-memory attempt state can decide this.
	b = New(b.cfg, b.store, b.engine, b.signer, local, b.hub, nil, nil)
	if err := b.checkGrantVisit(context.Background(), c, false); err != nil {
		t.Fatalf("retry cannot exchange/use grant: %v", err)
	}
	second := uploadAttemptTranscript(t, b, c, token, "attempt two")
	if data, err := local.Get(context.Background(), second); err != nil || string(data) != "attempt two" {
		t.Fatalf("retry's final was silently discarded: %q %v", data, err)
	}
	// The last preview is deliberately from the old attempt. Result acceptance
	// must select the submitted key, not whichever upload most recently arrived.
	uploadAttemptTranscript(t, b, c, token, "attempt one")
	w := attemptResult(t, b, token, second, "attempt two won")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("retry result: %d %s", w.Code, w.Body)
	}
	b = New(b.cfg, b.store, b.engine, b.signer, local, b.hub, nil, nil)
	for _, key := range []string{second, first} {
		w := attemptResult(t, b, token, key, "duplicate or stale")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":false`) {
			t.Fatalf("duplicate result: %d %s", w.Code, w.Body)
		}
	}
	uploadAttemptTranscript(t, b, c, token, "attempt two")
	uploadAttemptTranscript(t, b, c, token, "attempt one")
	uploadAttemptTranscript(t, b, c, token, "different late final")
	w = runtimeRequest(b.Handler(), "POST", "/v1/transcript", token, strings.NewReader("late periodic"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("late periodic status: %d", w.Code)
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.TranscriptKey != second || v.Summary != "attempt two won" {
		t.Fatalf("accepted result changed: %+v", v)
	}
	if data, err := local.Get(context.Background(), v.TranscriptKey); err != nil || string(data) != "attempt two" {
		t.Fatalf("accepted artifact changed: %q %v", data, err)
	}
}

func TestResultWaitsForOldUploadThenBindsOwnTranscript(t *testing.T) {
	b, c, token := runtimeBroker(t)
	b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
	local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.obj = local
	key := uploadAttemptTranscript(t, b, c, token, "accepted attempt")
	obj := &delayedTranscriptStore{Store: local, started: make(chan struct{}), release: make(chan struct{})}
	b.obj = obj
	var once sync.Once
	release := func() { once.Do(func() { close(obj.release) }) }
	defer release()
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		runtimeRequest(b.Handler(), "POST", "/v1/transcript", token, strings.NewReader("periodic"))
	}()
	select {
	case <-obj.started:
	case <-time.After(time.Second):
		t.Fatal("old upload did not start")
	}
	resultDone := make(chan struct{})
	var result *httptest.ResponseRecorder
	go func() { defer close(resultDone); result = attemptResult(t, b, token, key, "accepted attempt") }()
	select {
	case <-resultDone:
		t.Error("result did not serialize with the in-flight upload")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	for _, done := range []chan struct{}{oldDone, resultDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler did not finish")
		}
	}
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"accepted":true`) {
		t.Fatalf("result: %d %s", result.Code, result.Body)
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.TranscriptKey != key || v.Status != store.VisitSucceeded {
		t.Fatalf("old upload replaced accepted selection: %+v", v)
	}
	b.transcriptMu.Lock()
	defer b.transcriptMu.Unlock()
	if len(b.transcriptWrites) != 0 {
		t.Fatalf("leaked result/upload locks: %d", len(b.transcriptWrites))
	}
}

func TestResultRejectsUnuploadedOrForeignTranscript(t *testing.T) {
	b, c, token := runtimeBroker(t)
	b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
	local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.obj = local
	corruptKey := protocol.FinalTranscriptKey(c.Run, c.Seq, c.Node, []byte("original"))
	if err := local.Put(context.Background(), corruptKey, []byte("different"), "application/x-ndjson"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		protocol.FinalTranscriptKey("another-run", c.Seq, c.Node, []byte("log")),
		protocol.FinalTranscriptKey(c.Run, c.Seq+1, c.Node, []byte("log")),
		protocol.FinalTranscriptKey(c.Run, c.Seq, "other-node", []byte("log")),
		transcriptPrefix(c) + ".jsonl",
		transcriptPrefix(c) + ".final-" + strings.Repeat("x", 64) + ".jsonl",
		protocol.FinalTranscriptKey(c.Run, c.Seq, c.Node, []byte("missing")),
		corruptKey,
	} {
		w := attemptResult(t, b, token, key, "invalid")
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid key %q: %d %s", key, w.Code, w.Body)
		}
	}
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != store.VisitPending || v.TranscriptKey != "" {
		t.Fatalf("invalid result changed visit: %+v", v)
	}
}

func TestResultWithoutTranscriptDoesNotInheritEarlierAttempt(t *testing.T) {
	b, c, token := runtimeBroker(t)
	b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
	local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.obj = local
	uploadAttemptTranscript(t, b, c, token, "earlier attempt")
	w := attemptResult(t, b, token, "", "final upload failed")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatalf("result: %d %s", w.Code, w.Body)
	}
	uploadAttemptTranscript(t, b, c, token, "earlier attempt")
	uploadAttemptTranscript(t, b, c, token, "late unrelated attempt")
	v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if v.TranscriptKey != "" {
		t.Fatalf("inherited an unselected artifact: %s", v.TranscriptKey)
	}
}

type lateCompletionStore struct {
	objstore.Store
	completeLate func() error
}

func (s *lateCompletionStore) Put(ctx context.Context, key string, data []byte, typ string) error {
	if s.completeLate == nil {
		// Model an object-store HTTP timeout that returns before the remote
		// operation completes. Keep the pending write under test control.
		s.completeLate = func() error { return s.Store.Put(context.Background(), key, data, typ) }
		return context.DeadlineExceeded
	}
	return s.Store.Put(ctx, key, data, typ)
}

func TestFinalTranscriptSurvivesStoreCompletionAfterTimeout(t *testing.T) {
	for _, oldFinalFlag := range []string{"", "true"} {
		t.Run("old-final="+oldFinalFlag, func(t *testing.T) {
			b, c, token := runtimeBroker(t)
			b.engine = engine.New(b.cfg, b.store, nil, b.hub, nil)
			local, err := objstore.New(context.Background(), config.ObjectStore{}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			obj := &lateCompletionStore{Store: local}
			b.obj = obj
			old := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader("old timed out"))
			old.Header.Set("Authorization", "Bearer "+token)
			old.Header.Set(protocol.TranscriptFinalHeader, oldFinalFlag)
			w := httptest.NewRecorder()
			b.Handler().ServeHTTP(w, old)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("store timeout status: %d", w.Code)
			}
			final := httptest.NewRequest("POST", "/v1/transcript", strings.NewReader("latest final"))
			final.Header.Set("Authorization", "Bearer "+token)
			final.Header.Set(protocol.TranscriptFinalHeader, "true")
			w = httptest.NewRecorder()
			b.Handler().ServeHTTP(w, final)
			if w.Code != http.StatusNoContent {
				t.Fatalf("final status: %d %s", w.Code, w.Body)
			}
			key := protocol.FinalTranscriptKey(c.Run, c.Seq, c.Node, []byte("latest final"))
			w = attemptResult(t, b, token, key, "accepted before old write completes")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":true`) {
				t.Fatalf("result: %d %s", w.Code, w.Body)
			}
			if err := obj.completeLate(); err != nil {
				t.Fatal(err)
			}
			v, err := b.store.GetVisit(context.Background(), c.Run, c.Seq)
			if err != nil {
				t.Fatal(err)
			}
			data, err := local.Get(context.Background(), v.TranscriptKey)
			if err != nil || string(data) != "latest final" {
				t.Fatalf("late store write corrupted final: %q, %v", data, err)
			}
		})
	}
}

func TestDeliveryGraceAndTerminalPermissionsStayNarrow(t *testing.T) {
	for _, state := range []string{"stale-active", "run-canceled", "wrong-node", "waiting", "terminal-expired-token", "terminal-non-pod", "terminal-old-visit"} {
		t.Run(state, func(t *testing.T) {
			b, c, token := runtimeBroker(t)
			ctx := context.Background()
			if err := b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"deadline": store.Now() - 30_000}); err != nil {
				t.Fatal(err)
			}
			var err error
			want := http.StatusForbidden
			switch state {
			case "stale-active":
				_, err = b.store.AddVisit(ctx, c.Run, c.Node, flow.TypeCheck)
			case "run-canceled":
				err = b.store.UpdateRun(ctx, c.Run, map[string]any{"status": store.RunCanceled})
			case "wrong-node":
				c.Node = "another"
				token, err = b.signer.Mint(*c, time.Hour)
			case "waiting":
				err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"status": store.VisitWaiting})
			default:
				err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"status": store.VisitSucceeded, "deadline": store.Now() - 120_000})
				if err != nil {
					t.Fatal(err)
				}
				switch state {
				case "terminal-expired-token":
					token, err = b.signer.Mint(*c, -time.Second)
					want = http.StatusUnauthorized
				case "terminal-non-pod":
					err = b.store.UpdateVisit(ctx, c.Run, c.Seq, map[string]any{"type": flow.TypeGate})
				case "terminal-old-visit":
					_, err = b.store.AddVisit(ctx, c.Run, c.Node, flow.TypeCheck)
					want = http.StatusNoContent
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, route := range []string{"/v1/result", "/v1/transcript"} {
				w := runtimeRequest(b.Handler(), "POST", route, token, strings.NewReader("{}"))
				expected := want
				if route == "/v1/result" && want == http.StatusNoContent {
					expected = http.StatusOK
				}
				if w.Code != expected {
					t.Errorf("%s: status=%d want=%d: %s", route, w.Code, expected, w.Body)
				}
				if expected == http.StatusOK && !strings.Contains(w.Body.String(), `"accepted":false`) {
					t.Errorf("terminal result was not idempotent: %s", w.Body)
				}
			}
			if err := b.checkGrantVisit(ctx, c, false); err == nil {
				t.Fatal("delivery permissions enabled execution")
			}
		})
	}
}

type repeatedByteReader struct{}

func (repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestTranscriptSizeLimit(t *testing.T) {
	b, _, token := runtimeBroker(t)
	obj := b.obj.(*runtimeObjects)
	for _, size := range []int64{protocol.MaxTranscriptBytes, protocol.MaxTranscriptBytes + 1} {
		body := io.LimitReader(repeatedByteReader{}, size)
		w := runtimeRequest(b.Handler(), "POST", "/v1/transcript", token, body)
		if size == protocol.MaxTranscriptBytes {
			if w.Code != 204 || obj.puts != 1 || obj.size != int(size) {
				t.Fatalf("exact limit: status=%d puts=%d size=%d", w.Code, obj.puts, obj.size)
			}
		} else if w.Code != 413 || obj.puts != 1 {
			t.Fatalf("oversize wasn't rejected before storage: status=%d puts=%d", w.Code, obj.puts)
		}
	}
}

func TestBundleApprovedSecretEnvNames(t *testing.T) {
	b, _, _ := runtimeBroker(t)
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Grants["secret/runtime-test"] = &config.Grant{Kind: config.GrantSecret, Env: "APPROVED_RUNTIME_TEST", SecretRef: &config.SecretRef{Name: "test", Key: "value"}}
	cfg.Catalog.Grants["secret/ungranted-test"] = &config.Grant{Kind: config.GrantSecret, Env: "UNGRANTED_RUNTIME_TEST", SecretRef: &config.SecretRef{Name: "test", Key: "other"}}
	cfg.Projects["sandbox"].Spec.Allow.Grants = append(cfg.Projects["sandbox"].Spec.Allow.Grants, "secret/runtime-test")
	b.cfg = cfg
	b.engine = engine.New(cfg, b.store, nil, b.hub, nil)
	ctx := context.Background()
	_, err = b.store.SaveFlow(ctx, &store.FlowVersion{Name: "secret-test", YAML: `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: secret-test, project: sandbox}
spec:
  start: check
  nodes:
    check:
      type: check
      run: "true"
      grants: [secret/runtime-test]
      next: {pass: $success, fail: $fail}
`})
	if err != nil {
		t.Fatal(err)
	}
	run, err := b.engine.CreateRun(ctx, "secret-test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.store.UpdateRun(ctx, run.ID, map[string]any{"status": store.RunRunning, "current_node": "check"}); err != nil {
		t.Fatal(err)
	}
	v, err := b.store.AddVisit(ctx, run.ID, "check", flow.TypeCheck)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := b.buildBundle(ctx, run.ID, v.Seq)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var received protocol.Bundle
	if err := json.Unmarshal(raw, &received); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(received.SecretEnv, []string{"APPROVED_RUNTIME_TEST"}) {
		t.Fatalf("approved names: %v", received.SecretEnv)
	}
}

// An agent step gets the harness's and the project's instructions and the
// model's thinking format; one without repo:write gets no file-editing tools.
func TestBundleAgentSetup(t *testing.T) {
	b, _, _ := runtimeBroker(t)
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Harnesses["pi"] = config.Harness{Instructions: "Verify first.", Settings: map[string]any{"compaction": map[string]any{"enabled": false}}}
	cfg.Projects["sandbox"].Spec.Agent.Instructions = "Run python -m unittest."
	m := cfg.Catalog.Models["gpt-6.1-sol"]
	m.Reasoning, m.ThinkingFormat, m.MaxOutputTokens = true, "reasoning_effort", 32000
	b.cfg = cfg
	b.engine = engine.New(cfg, b.store, nil, b.hub, nil)
	ctx := context.Background()
	if _, err := b.store.SaveFlow(ctx, &store.FlowVersion{Name: "agent-test", YAML: `apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: {name: agent-test, project: sandbox}
spec:
  start: look
  nodes:
    look:
      type: agent
      model: gpt-6.1-sol
      prompt: look
      outcomes: [done]
      next: {done: fix}
    fix:
      type: agent
      model: gpt-6.1-sol
      prompt: fix
      grants: [repo/ai-flow-sandbox:write]
      outcomes: [done]
      next: {done: $success}
`}); err != nil {
		t.Fatal(err)
	}
	run, err := b.engine.CreateRun(ctx, "agent-test", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string][]string{}
	for _, node := range []string{"look", "fix"} {
		if err := b.store.UpdateRun(ctx, run.ID, map[string]any{"status": store.RunRunning, "current_node": node}); err != nil {
			t.Fatal(err)
		}
		v, err := b.store.AddVisit(ctx, run.ID, node, flow.TypeAgent)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := b.buildBundle(ctx, run.ID, v.Seq)
		if err != nil {
			t.Fatal(err)
		}
		tools[node] = bundle.Tools
		a := bundle.Agent
		if a == nil || a.Instructions != "Verify first." || a.ProjectInstructions != "Run python -m unittest." || a.Settings["compaction"] == nil {
			t.Fatalf("%s: agent setup %+v", node, a)
		}
		if mi := bundle.LLM.Models[0]; mi.ThinkingFormat != "reasoning_effort" || mi.MaxOutputTokens != 32000 {
			t.Fatalf("%s: model %+v", node, mi)
		}
	}
	if slices.Contains(tools["look"], "edit") || slices.Contains(tools["look"], "write") || !slices.Contains(tools["look"], "read") {
		t.Errorf("read-only step tools %v", tools["look"])
	}
	if !slices.Contains(tools["fix"], "edit") || !slices.Contains(tools["fix"], "write") {
		t.Errorf("writing step tools %v", tools["fix"])
	}
}
