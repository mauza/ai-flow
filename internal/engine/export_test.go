package engine

import (
	"context"

	"github.com/mauza/ai-flow/internal/store"
)

// ShortDuration exposes shortDuration to the external test package.
var ShortDuration = shortDuration

// RunDependencies fingerprints a run's pinned execution with a given snapshot
// version, for the external test package.
func (e *Engine) RunDependencies(ctx context.Context, r *store.Run, version int) (string, error) {
	snap, err := e.loadSnapshot(ctx, r)
	if err != nil {
		return "", err
	}
	return executionDependencies(snap.Resolved, e.cfg.Current(), version)
}

// SnapshotVersion is the version new runs pin.
const SnapshotVersion = snapshotVersion
