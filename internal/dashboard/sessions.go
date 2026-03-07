package dashboard

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/subprocess"
)

const (
	maxBufBytes    = 1 << 20 // 1 MB accumulated output buffer
	subChanBufSize = 512     // buffered channel per SSE subscriber
)

// OutputEvent is a single chunk of output from a subprocess.
type OutputEvent struct {
	Type string    `json:"type"` // "stdout" | "stderr" | "system"
	Data string    `json:"data"`
	Time time.Time `json:"time"`
}

// Session tracks an active subprocess run.
type Session struct {
	RunID           int64
	IssueID         string
	IssueIdentifier string
	IssueTitle      string
	IssueURL        string
	StageName       string
	StartedAt       time.Time
	cancel          context.CancelFunc

	mu          sync.Mutex
	buf         []OutputEvent
	bufBytes    int
	subscribers []chan OutputEvent
	Done        chan struct{} // closed when subprocess ends
}

// sessionWriter is an io.Writer that forwards data to a Session as OutputEvents.
type sessionWriter struct {
	session *Session
	evtType string
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.session.appendEvent(OutputEvent{
		Type: w.evtType,
		Data: string(p),
		Time: time.Now(),
	})
	return len(p), nil
}

// appendEvent stores an event and fans it out to all subscribers.
func (s *Session) appendEvent(evt OutputEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.bufBytes < maxBufBytes {
		s.buf = append(s.buf, evt)
		s.bufBytes += len(evt.Data)
	}

	for _, ch := range s.subscribers {
		select {
		case ch <- evt:
		default: // drop if subscriber is slow rather than blocking the subprocess
		}
	}
}

// subscribe returns a snapshot of the current buffer and a channel for future events.
func (s *Session) subscribe() ([]OutputEvent, chan OutputEvent) {
	ch := make(chan OutputEvent, subChanBufSize)
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := make([]OutputEvent, len(s.buf))
	copy(snapshot, s.buf)
	s.subscribers = append(s.subscribers, ch)
	return snapshot, ch
}

// unsubscribe removes a subscriber channel.
func (s *Session) unsubscribe(ch chan OutputEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subscribers {
		if sub == ch {
			s.subscribers = append(s.subscribers[:i], s.subscribers[i+1:]...)
			return
		}
	}
}

// SessionSummary is the JSON-serializable view of a session for the list API.
type SessionSummary struct {
	RunID           int64     `json:"run_id"`
	IssueID         string    `json:"issue_id"`
	IssueIdentifier string    `json:"issue_identifier"`
	IssueTitle      string    `json:"issue_title"`
	IssueURL        string    `json:"issue_url"`
	StageName       string    `json:"stage_name"`
	StartedAt       time.Time `json:"started_at"`
}

// SessionDetail includes the output buffer.
type SessionDetail struct {
	SessionSummary
	Output []OutputEvent `json:"output"`
}

// Registry holds all active subprocess sessions.
type Registry struct {
	mu       sync.RWMutex
	sessions map[int64]*Session
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{sessions: make(map[int64]*Session)}
}

// TrackStart implements subprocess.OutputTracker.
// It registers a new session, emits the composed prompt as a "stdin" event,
// and returns stdout/stderr writers for live streaming.
func (r *Registry) TrackStart(runID int64, input subprocess.Input, prompt string, cancel context.CancelFunc) (io.Writer, io.Writer) {
	s := &Session{
		RunID:           runID,
		IssueID:         input.IssueID,
		IssueIdentifier: input.IssueIdentifier,
		IssueTitle:      input.IssueTitle,
		IssueURL:        input.IssueURL,
		StageName:       input.StageName,
		StartedAt:       time.Now(),
		cancel:          cancel,
		Done:            make(chan struct{}),
	}

	// Emit the composed prompt so the dashboard can show what was sent.
	// This stays in the in-memory buffer only — never written to the DB.
	s.appendEvent(OutputEvent{
		Type: "stdin",
		Data: prompt,
		Time: time.Now(),
	})

	r.mu.Lock()
	r.sessions[runID] = s
	r.mu.Unlock()

	return &sessionWriter{session: s, evtType: "stdout"},
		&sessionWriter{session: s, evtType: "stderr"}
}

// TrackEnd implements subprocess.OutputTracker.
// It marks the session as done and schedules its removal.
func (r *Registry) TrackEnd(runID int64) {
	r.mu.RLock()
	s, ok := r.sessions[runID]
	r.mu.RUnlock()
	if !ok {
		return
	}

	s.appendEvent(OutputEvent{
		Type: "system",
		Data: "[process ended]",
		Time: time.Now(),
	})
	close(s.Done)

	// Keep session briefly so SSE clients can receive the final events
	go func() {
		time.Sleep(10 * time.Second)
		r.mu.Lock()
		delete(r.sessions, runID)
		r.mu.Unlock()
	}()
}

// Get returns the session for a run ID, or nil if not found.
func (r *Registry) Get(runID int64) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[runID]
}

// List returns all active sessions.
func (r *Registry) List() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

// Kill cancels the subprocess context for the given run.
func (r *Registry) Kill(runID int64) bool {
	r.mu.RLock()
	s, ok := r.sessions[runID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	s.cancel()
	return true
}

// Subscribe returns the current output snapshot and a channel for future events.
// Call Unsubscribe when done.
func (r *Registry) Subscribe(runID int64) ([]OutputEvent, chan OutputEvent, *Session, bool) {
	r.mu.RLock()
	s, ok := r.sessions[runID]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, nil, false
	}
	snapshot, ch := s.subscribe()
	return snapshot, ch, s, true
}

// Unsubscribe removes a subscriber channel from the session.
func (r *Registry) Unsubscribe(runID int64, ch chan OutputEvent) {
	r.mu.RLock()
	s, ok := r.sessions[runID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	s.unsubscribe(ch)
}
