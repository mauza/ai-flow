package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/protocol"
	"github.com/mauza/ai-flow/internal/store"
)

type transcriptWrite struct {
	gate chan struct{}
	refs int // active writer plus waiters; guarded by transcriptMu
}

func transcriptPrefix(c *grant.Claims) string {
	return fmt.Sprintf("runs/%s/%03d-%s", c.Run, c.Seq, c.Node)
}

// A result can select only a complete, content-addressed upload for its visit.
// Check the namespace before touching the object store; a grant is not an
// arbitrary object-store read capability.
func validResultTranscriptKey(c *grant.Claims, key string) bool {
	digest, ok := strings.CutPrefix(key, transcriptPrefix(c)+".final-")
	if !ok || !strings.HasSuffix(digest, ".jsonl") {
		return false
	}
	digest = strings.TrimSuffix(digest, ".jsonl")
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// Serialize publication per visit, including a Put that ignores cancellation.
// Waiting is context-aware; entries disappear when their last request leaves.
// There is no background goroutine or permanent per-run lock/version map.
func (b *Broker) lockTranscript(ctx context.Context, key string) (func(), error) {
	b.transcriptMu.Lock()
	if b.transcriptWrites == nil {
		b.transcriptWrites = make(map[string]*transcriptWrite)
	}
	s := b.transcriptWrites[key]
	if s == nil {
		s = &transcriptWrite{gate: make(chan struct{}, 1)}
		b.transcriptWrites[key] = s
	}
	s.refs++
	b.transcriptMu.Unlock()
	unref := func() {
		b.transcriptMu.Lock()
		defer b.transcriptMu.Unlock()
		s.refs--
		if s.refs == 0 {
			delete(b.transcriptWrites, key)
		}
	}
	select {
	case s.gate <- struct{}{}:
		return func() { <-s.gate; unref() }, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

func (b *Broker) transcript(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	finalFlag := r.Header.Get(protocol.TranscriptFinalHeader)
	if finalFlag != "" && finalFlag != "true" {
		httpErr(w, http.StatusBadRequest, "invalid transcript final flag")
		return
	}
	ctx := r.Context()
	prefix := transcriptPrefix(c)
	unlock, err := b.lockTranscript(ctx, prefix)
	if err != nil {
		httpErr(w, http.StatusRequestTimeout, err.Error())
		return
	}
	defer unlock()
	// Do not buffer another transcript while this visit's previous write is
	// still in progress. Revalidate after waiting, before accepting any data.
	if err := b.checkGrantVisit(ctx, c, true); err != nil {
		httpErr(w, http.StatusForbidden, err.Error())
		return
	}
	v, err := b.store.GetVisit(ctx, c.Run, c.Seq)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if v.Status != store.VisitPending && v.Status != store.VisitRunning {
		// Completion freezes the selection, including an explicitly empty key.
		// A prior final upload alone does not freeze an active/retryable visit.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, protocol.MaxTranscriptBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpErr(w, http.StatusRequestEntityTooLarge, "transcript exceeds 64 MiB")
			return
		}
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := ctx.Err(); err != nil {
		httpErr(w, http.StatusRequestTimeout, err.Error())
		return
	}
	key := prefix + ".jsonl"
	if finalFlag == "true" {
		// A remote store may complete a timed-out Put even after it returns.
		// Give the final immutable content its own key so such writes cannot
		// corrupt the final object, even if an earlier final attempt timed out.
		key = protocol.FinalTranscriptKey(c.Run, c.Seq, c.Node, data)
	}
	if err := b.obj.Put(ctx, key, data, "application/x-ndjson"); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// This is only a live preview. Results share this lock and atomically bind
	// their own key with terminal status. The condition also protects against
	// engine completion, which does not acquire the broker's per-visit lock.
	if _, err := b.store.UpdateVisitIf(ctx, c.Run, c.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{"transcript_key": key}); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
