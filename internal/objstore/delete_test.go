package objstore

import (
	"context"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

func TestLocalDeletePrefixStaysInsideItsPrefix(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.ObjectStore{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := s.(Deleter)
	s.Put(ctx, "runs/r-1/a", []byte("x"), "")
	s.Put(ctx, "runs/r-12/a", []byte("y"), "")
	for _, bad := range []string{"", "/", "runs/r-1", "../", "runs/../../"} {
		if err := d.DeletePrefix(ctx, bad); err == nil {
			t.Errorf("DeletePrefix(%q) should be refused", bad)
		}
	}
	if err := d.DeletePrefix(ctx, "runs/r-1/"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "runs/r-1/a"); err != ErrNotFound {
		t.Errorf("r-1 survived: %v", err)
	}
	if _, err := s.Get(ctx, "runs/r-12/a"); err != nil {
		t.Errorf("r-12 was removed: %v", err)
	}
}
