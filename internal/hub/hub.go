// Package hub fans out change notifications to UI clients (server-sent events).
package hub

import (
	"sync"
)

// Event says that something changed; clients refetch what they display.
type Event struct {
	Type string `json:"type"` // run | visit | task | flow | progress
	ID   string `json:"id"`   // run id, task id or flow name
	Seq  int    `json:"seq,omitempty"`
	Text string `json:"text,omitempty"` // progress line
}

type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func New() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // slow client: drop; it refetches on the next event
		}
	}
}
