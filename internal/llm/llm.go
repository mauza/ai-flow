// Package llm is a minimal OpenAI-compatible chat client for the control
// plane's own calls (the planner). Node pods go through the broker instead.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/antigravity"
	"github.com/mauza/ai-flow/internal/config"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type Client struct {
	cfg     *config.Config
	http    *http.Client
	OnUsage func(ctx context.Context, model string, in, out, cached int64)
	OnLimit func(upstream string, retryAfter time.Duration)
}

func New(cfg *config.Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 15 * time.Minute}}
}

type Options struct {
	Temperature float64
	MaxTokens   int
	Stream      bool
}

// Chat sends messages to a catalog model and returns the reply text.
func (c *Client) Chat(ctx context.Context, model string, msgs []Message, opt Options) (string, Usage, error) {
	m := c.cfg.Current().Catalog.Models[model]
	if m == nil {
		return "", Usage{}, fmt.Errorf("unknown model %q", model)
	}
	up, ok := c.cfg.Current().Env.LLM.Upstreams[m.Upstream]
	if !ok {
		return "", Usage{}, fmt.Errorf("model %s: unknown upstream %q", model, m.Upstream)
	}
	if up.IsAntigravity() {
		return c.chatAntigravity(ctx, model, m, up, msgs)
	}
	body := map[string]any{"model": m.Model, "messages": msgs, "temperature": opt.Temperature}
	if opt.MaxTokens > 0 {
		body["max_tokens"] = opt.MaxTokens
	}
	if opt.Stream {
		body["stream"] = true
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	raw, _ := json.Marshal(body)
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			case <-ctx.Done():
				return "", Usage{}, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(up.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return "", Usage{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		if opt.Stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		if key := config.Secret(up.APIKeyEnv); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", Usage{}, ctx.Err()
			}
			lastErr = err
			continue
		}
		if opt.Stream {
			if resp.StatusCode >= 300 {
				// Error bodies can contain credentials or echoed prompt content.
				ra := parseRetryAfter(resp.Header.Get("Retry-After"))
				resp.Body.Close()
				if resp.StatusCode == 429 && c.OnLimit != nil {
					c.OnLimit(m.Upstream, ra)
				}
				lastErr = fmt.Errorf("%s: HTTP %d", model, resp.StatusCode)
				if resp.StatusCode == 429 || resp.StatusCode >= 500 {
					continue
				}
				return "", Usage{}, lastErr
			}
			reply, usage, err := ReadStream(ctx, resp.Body)
			resp.Body.Close()
			if err != nil {
				return "", Usage{}, fmt.Errorf("%s: %w", model, err)
			}
			if c.OnUsage != nil {
				c.OnUsage(ctx, model, usage.PromptTokens, usage.CompletionTokens, 0)
			}
			return reply, usage, nil
		}
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 && c.OnLimit != nil {
			c.OnLimit(m.Upstream, ra)
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: HTTP %d: %s", model, resp.StatusCode, truncate(string(data), 300))
			continue
		}
		if resp.StatusCode >= 300 {
			return "", Usage{}, fmt.Errorf("%s: HTTP %d: %s", model, resp.StatusCode, truncate(string(data), 500))
		}
		if readErr != nil {
			return "", Usage{}, fmt.Errorf("%s: reading response: %w", model, readErr)
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage Usage `json:"usage"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return "", Usage{}, fmt.Errorf("%s: bad response: %w", model, err)
		}
		if len(out.Choices) == 0 {
			return "", out.Usage, fmt.Errorf("%s: no choices in response", model)
		}
		if c.OnUsage != nil {
			c.OnUsage(ctx, model, out.Usage.PromptTokens, out.Usage.CompletionTokens, 0)
		}
		return out.Choices[0].Message.Content, out.Usage, nil
	}
	return "", Usage{}, lastErr
}

func (c *Client) chatAntigravity(ctx context.Context, model string, m *config.Model, up config.Upstream, msgs []Message) (string, Usage, error) {
	agyMsgs := make([]antigravity.Message, len(msgs))
	for i, msg := range msgs {
		agyMsgs[i] = antigravity.Message{Role: msg.Role, Content: msg.Content}
	}
	req := antigravity.CompleteRequest{
		Binary:   up.Binary,
		Model:    m.Model,
		Messages: agyMsgs,
		AuthEnv:  up.AuthEnv,
		AuthFile: up.AuthFile,
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 5 * time.Second):
			case <-ctx.Done():
				return "", Usage{}, ctx.Err()
			}
		}
		resp, err := antigravity.Complete(ctx, req)
		if err == nil {
			if c.OnUsage != nil {
				c.OnUsage(ctx, model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.CachedTokens)
			}
			return resp.Content, Usage{
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
			}, nil
		}
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		var aerr *antigravity.Error
		if errors.As(err, &aerr) {
			if (aerr.RateLimit || aerr.QuotaLimit) && c.OnLimit != nil {
				c.OnLimit(m.Upstream, 0)
			}
			if aerr.AuthError || (!aerr.RateLimit && !aerr.QuotaLimit && aerr.Status != "") {
				return "", Usage{}, fmt.Errorf("%s: %w", model, err)
			}
		}
		lastErr = fmt.Errorf("%s: %w", model, err)
	}
	return "", Usage{}, lastErr
}

func parseRetryAfter(h string) time.Duration {
	if s := strings.TrimSpace(h); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
