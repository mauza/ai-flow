package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/protocol"
)

const (
	maxCapturedTranscript    = 4 << 20
	maxPiEventBytes          = 1 << 20
	transcriptUploadInterval = 15 * time.Second
	transcriptUploadTimeout  = time.Minute
)

// Keep a prefix of complete events, reserving space for visible loss metadata.
// One collector and one uploader snapshot are retained, regardless of duration
// or upload speed. Once truncated, later events are counted but not retained.
type transcriptCapture struct {
	mu            sync.Mutex
	data          []byte
	droppedBytes  int64
	droppedEvents int64
	version       uint64
}

func (t *transcriptCapture) record(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.version++
	if t.droppedEvents > 0 || len(line)+1 > maxCapturedTranscript-256-len(t.data) {
		t.droppedBytes += int64(len(line) + 1)
		t.droppedEvents++
		return
	}
	if t.data == nil {
		t.data = make([]byte, 0, maxCapturedTranscript)
	}
	t.data = append(t.data, line...)
	t.data = append(t.data, '\n')
}

func (t *transcriptCapture) drop(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.version++
	t.droppedEvents++
	t.droppedBytes += n
}

func (t *transcriptCapture) snapshot(after uint64) ([]byte, uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.version == after {
		return nil, after
	}
	var marker string
	if t.droppedEvents > 0 {
		marker = fmt.Sprintf("{\"type\":\"transcript_truncated\",\"limit_bytes\":%d,\"event_limit_bytes\":%d,\"dropped_events\":%d,\"dropped_bytes\":%d}\n",
			maxCapturedTranscript, maxPiEventBytes, t.droppedEvents, t.droppedBytes)
	}
	out := make([]byte, 0, len(t.data)+len(marker))
	out = append(out, t.data...)
	out = append(out, marker...)
	return out, t.version
}

func (t *transcriptCapture) event(event any) {
	line, err := json.Marshal(event)
	if err == nil {
		t.record(line)
	}
}

// startTranscriptUploads coalesces updates by taking a snapshot only after the
// preceding upload has finished. Stop stops scheduling, waits for the bounded
// in-flight request, then sends the final snapshot with its own deadline.
// Worst-case shutdown is two upload timeouts. The broker guards publication
// against writes that outlive an HTTP timeout; joining this client cannot.
func (r *Runner) startTranscriptUploads(t *transcriptCapture, interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var uploaded uint64
		for {
			select {
			case <-stop:
				return
			default:
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
				data, version := t.snapshot(uploaded)
				if len(data) == 0 {
					continue
				}
				if err := r.sendTranscript(context.Background(), data, false); err == nil {
					uploaded = version
				} else {
					slog.Warn("upload transcript", "err", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		data, _ := t.snapshot(0)
		r.uploadTranscript(data)
	}
}

func (r *Runner) sendTranscript(ctx context.Context, data []byte, final bool) error {
	ctx, cancel := context.WithTimeout(ctx, transcriptUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", r.podURL+"/v1/transcript", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Authorization", "Bearer "+r.b.Grant)
	if final {
		req.Header.Set(protocol.TranscriptFinalHeader, "true")
	}
	return r.do(req, nil)
}

// piEventWriter drains arbitrarily long lines without retaining them. Using it
// as Cmd.Stdout lets os/exec's WaitDelay bound pipes held open by descendants.
type piEventWriter struct {
	r          *Runner
	transcript *transcriptCapture
	line       []byte
	size       int64
}

func (w *piEventWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		w.size += int64(len(part))
		if w.size <= maxPiEventBytes {
			if w.line == nil {
				w.line = make([]byte, 0, maxPiEventBytes)
			}
			w.line = append(w.line, part...)
		}
		if i < 0 {
			break
		}
		w.finishLine()
		p = p[i+1:]
	}
	return n, nil
}

func (w *piEventWriter) finishLine() {
	if w.size > maxPiEventBytes {
		w.transcript.drop(w.size + 1)
	} else {
		w.transcript.record(w.line)
		w.r.piProgress(w.line)
	}
	w.line = w.line[:0]
	w.size = 0
}
