package objstore

import (
	"context"
	"os"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

func TestLocalRoundTrip(t *testing.T) {
	s, err := New(context.Background(), config.ObjectStore{Type: "local"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "runs/r-1/001-a.jsonl", []byte("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	b, err := s.Get(ctx, "runs/r-1/001-a.jsonl")
	if err != nil || string(b) != "hello" {
		t.Fatalf("got %q %v", b, err)
	}
	if _, err := s.Get(ctx, "runs/missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Put(ctx, "../escape", nil, ""); err == nil {
		t.Fatal("path escape allowed")
	}
}

// Integration test against a real Garage v2 (see Makefile target test-garage).
// GARAGE_TEST_ADMIN=http://localhost:13903 GARAGE_TEST_S3=localhost:13900 GARAGE_ADMIN_TOKEN=...
func TestGarageBootstrap(t *testing.T) {
	admin, s3ep := os.Getenv("GARAGE_TEST_ADMIN"), os.Getenv("GARAGE_TEST_S3")
	if admin == "" {
		t.Skip("GARAGE_TEST_ADMIN not set")
	}
	cfg := config.ObjectStore{Type: "garage", Endpoint: s3ep, Bucket: "ai-flow-test", Region: "garage",
		AdminEndpoint: admin, AdminTokenEnv: "GARAGE_ADMIN_TOKEN"}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // bootstrap must be idempotent
		s, err := New(ctx, cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, "k", []byte("v"), "text/plain"); err != nil {
			t.Fatal(err)
		}
		if b, err := s.Get(ctx, "k"); err != nil || string(b) != "v" {
			t.Fatalf("got %q %v", b, err)
		}
	}
}
