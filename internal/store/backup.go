package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Snapshot writes a consistent copy of the database to dst and verifies it.
//
// The database runs in EXCLUSIVE locking mode, so no other process can read it
// while the server is up; an outside dump job would only ever see "locked".
// VACUUM INTO runs on the server's own connection, which holds the lock.
func (s *Store) Snapshot(ctx context.Context, dst string) error {
	tmp := dst + ".partial"
	_ = os.Remove(tmp) // VACUUM INTO refuses an existing file
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	if err := checkIntegrity(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func checkIntegrity(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity check: %s", res)
	}
	return nil
}

// Backups snapshots the database into dir once a day (and shortly after
// start), keeping the newest keep files. It returns when ctx is done.
func (s *Store) Backups(ctx context.Context, dir string, keep int) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("backups disabled", "dir", dir, "err", err)
		return
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// One file per day: a restart the same day replaces it.
		dst := filepath.Join(dir, "ai-flow-"+time.Now().UTC().Format("20060102")+".db")
		if err := s.Snapshot(ctx, dst); err != nil {
			slog.Error("backup failed", "dst", dst, "err", err)
		} else {
			slog.Info("backup written", "dst", dst)
			prune(dir, keep)
		}
		t.Reset(24 * time.Hour)
	}
}

func prune(dir string, keep int) {
	matches, _ := filepath.Glob(filepath.Join(dir, "ai-flow-*.db"))
	// Names embed the date, so lexical order is age order.
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for i, m := range matches {
		if i >= keep {
			_ = os.Remove(m)
		}
	}
}
