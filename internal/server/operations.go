package server

import (
	"context"
	"time"

	"github.com/mauza/ai-flow/internal/store"
)

type operationsView = store.Operations

// Aggregate in SQLite so historical snapshots and transcripts are never loaded
// merely to display queue counts and per-node timings.
func (s *Server) operations(ctx context.Context) (*operationsView, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.app.Store.Operations(ctx)
}
