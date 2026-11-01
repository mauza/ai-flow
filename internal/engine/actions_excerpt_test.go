package engine

import (
	"strings"
	"testing"
)

func TestFailureExcerpt(t *testing.T) {
	var log strings.Builder
	for i := 0; i < 300; i++ {
		log.WriteString("2026-10-31T21:27:20.0755383Z ok 1 - fine\n")
	}
	log.WriteString("2026-10-31T21:27:22.8603622Z \x1b[31mnot ok 285 - saved result reopens\n2026-10-31T21:27:22.8619696Z   error: \"Cannot read properties of undefined\"\n")
	for i := 0; i < 300; i++ {
		log.WriteString("2026-10-31T21:27:23.0000000Z ok 2 - fine\n")
	}
	got := failureExcerpt(log.String(), 3000)
	if !strings.Contains(got, "not ok 285") || !strings.Contains(got, "Cannot read") || strings.Contains(got, "2026-10-31T") || strings.Contains(got, "\x1b") || strings.Count(got, "ok 1 - fine") > 2 {
		t.Fatalf("excerpt:\n%s", got)
	}
	if tail := failureExcerpt("a\nb\nc", 3000); tail != "a\nb\nc" {
		t.Fatalf("no failure lines: want the tail, got %q", tail)
	}
}
