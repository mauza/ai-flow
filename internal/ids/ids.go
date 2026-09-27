// Package ids makes short random identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

// New returns prefix + 10 hex chars, e.g. "r-3f9a1c02be".
func New(prefix string) string {
	b := make([]byte, 5)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns free text into a lowercase-dash name of at most max chars.
func Slug(s string, max int) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > max {
		s = strings.Trim(s[:max], "-")
	}
	if s == "" {
		s = "flow"
	}
	return s
}
