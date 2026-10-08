package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The live database is locked against every other process, so the snapshot
// must come from the server's own connection and open on its own.
func TestSnapshotWhileLocked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO kv (key, value) VALUES ('k', 'v')`); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "snap.db")
	if err := s.Snapshot(ctx, dst); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := s.Snapshot(ctx, dst); err != nil {
		t.Fatalf("second Snapshot over an existing file: %v", err)
	}
	if _, err := os.Stat(dst + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind (err=%v)", err)
	}

	snap, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	var v string
	if err := snap.db.QueryRow(`SELECT value FROM kv WHERE key = 'k'`).Scan(&v); err != nil || v != "v" {
		t.Fatalf("snapshot kv = %q, err %v", v, err)
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"20261001", "20261003", "20261002"} {
		if err := os.WriteFile(filepath.Join(dir, "ai-flow-"+d+".db"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prune(dir, 2)
	got, _ := filepath.Glob(filepath.Join(dir, "ai-flow-*.db"))
	if len(got) != 2 || filepath.Base(got[0]) != "ai-flow-20261002.db" || filepath.Base(got[1]) != "ai-flow-20261003.db" {
		t.Fatalf("after prune: %v", got)
	}
}
