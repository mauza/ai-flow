package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExclusiveWALHasNoShm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%q err=%v", mode, err)
	}
	if _, err := os.Stat(p + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("-shm file exists (err=%v); WAL index must stay in memory", err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("second opener got the database while the first holds it")
	}
}
