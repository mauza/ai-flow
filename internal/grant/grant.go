// Package grant mints and verifies the bearer tokens node pods use for the
// pod-facing API (git, LLM, MCP, results). Only the control plane verifies
// them, so a symmetric key is enough.
package grant

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims describe exactly what one node visit may do.
type Claims struct {
	Kind   string   `json:"kind"` // "grant" or "launch"
	Run    string   `json:"run"`
	Seq    int      `json:"seq"`
	Node   string   `json:"node"`
	Repo   string   `json:"repo,omitempty"` // host/owner/repo
	Write  bool     `json:"write,omitempty"`
	Branch string   `json:"branch,omitempty"`
	Models []string `json:"models,omitempty"` // catalog names
	MCP    []string `json:"mcp,omitempty"`    // server/tool
	jwt.RegisteredClaims
}

type Signer struct {
	key []byte
}

// LoadOrCreate reads the signing key from dir/grant.key, creating it on first use.
func LoadOrCreate(dir string) (*Signer, error) {
	path := filepath.Join(dir, "grant.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		b = []byte(hex.EncodeToString(raw))
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &Signer{key: b}, nil
}

func New(key []byte) *Signer { return &Signer{key: key} }

func (s *Signer) Mint(c Claims, ttl time.Duration) (string, error) {
	now := time.Now()
	c.IssuedAt = jwt.NewNumericDate(now)
	c.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	c.Issuer = "ai-flow"
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.key)
}

func (s *Signer) Verify(token, kind string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.key, nil
	}, jwt.WithIssuer("ai-flow"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	if c.Kind != kind {
		return nil, fmt.Errorf("token kind %q, want %q", c.Kind, kind)
	}
	return &c, nil
}

func (c *Claims) AllowsModel(name string) bool {
	for _, m := range c.Models {
		if m == name {
			return true
		}
	}
	return false
}

func (c *Claims) AllowsMCP(server, tool string) bool {
	for _, t := range c.MCP {
		if t == server+"/"+tool {
			return true
		}
	}
	return false
}
