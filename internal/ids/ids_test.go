package ids

import "testing"

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Play Stack Pulse, a one-tap timing game":          "play-stack-pulse-a-one-tap",
		"MAU-22 Fix it":                                    "mau-22-fix-it",
		"supercalifragilisticexpialidocious-and-more-text": "supercalifragilisticexpial",
		"exactly-twenty-six-chars-x":                       "exactly-twenty-six-chars-x",
		"!!!":                                              "flow",
	} {
		if got := Slug(in, 26); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
