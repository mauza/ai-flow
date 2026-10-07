package server

import (
	"context"
	"net/http"
	"time"
)

const readinessTimeout = 750 * time.Millisecond

// SetBrokerReady wires local listener availability. Set before serving requests;
// the callback must be concurrency-safe and nonblocking.
func (s *Server) SetBrokerReady(ready func() bool) { s.brokerReady = ready }

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	database := s.app.Store != nil && s.app.Store.Ping(ctx) == nil
	broker := s.brokerReady != nil && s.brokerReady()
	status := http.StatusOK
	if !database || !broker {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":  database && broker,
		"checks": map[string]bool{"database": database, "broker": broker},
	})
}
