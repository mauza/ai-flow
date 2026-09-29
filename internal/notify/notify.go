// Package notify pushes short messages to a human when a run needs them.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
)

// Message is one push notification. Click opens the run in the UI.
type Message struct {
	Title    string
	Body     string
	Click    string
	Priority int      // 1 (min) … 5 (max); 0 = server default
	Tags     []string // ntfy renders known tags as emoji
}

// Sender delivers a message.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// New returns the configured sender, or nil when notifications are off.
func New(cfg config.Notify) Sender {
	if cfg.Ntfy == nil {
		return nil
	}
	return &Ntfy{URL: strings.TrimSuffix(cfg.Ntfy.URL, "/"), Topic: cfg.Ntfy.Topic, Token: config.Secret(cfg.Ntfy.TokenEnv), Client: &http.Client{Timeout: 10 * time.Second}}
}

// Ntfy publishes through ntfy's JSON API (POST to the server root).
type Ntfy struct {
	URL, Topic, Token string
	Client            *http.Client
}

func (n *Ntfy) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]any{
		"topic": n.Topic, "title": m.Title, "message": m.Body, "click": m.Click,
		"priority": m.Priority, "tags": m.Tags,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
