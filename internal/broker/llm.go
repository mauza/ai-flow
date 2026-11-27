package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/antigravity"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/runner"
)

func (b *Broker) llmModels(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	var data []map[string]any
	for _, m := range c.Models {
		data = append(data, map[string]any{"id": m, "object": "model"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// llmChat proxies one chat completion to the model's upstream. The pod names
// catalog models; only models in its grant are allowed. Budgets are checked
// before each call and usage is recorded after it.
func (b *Broker) llmChat(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	b.llmProxy(w, r, c, "/chat/completions")
}

// llmResponses is llmChat for the Responses API (codex speaks nothing else).
func (b *Broker) llmResponses(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	b.llmProxy(w, r, c, "/responses")
}

func (b *Broker) llmProxy(w http.ResponseWriter, r *http.Request, c *grant.Claims, path string) {
	ctx := r.Context()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		httpErr(w, 400, "body must be JSON")
		return
	}
	name, _ := body["model"].(string)
	if !c.AllowsModel(name) {
		limitErr(w, 403, "model_not_allowed", fmt.Sprintf("model %q is not granted to this node (allowed: %s)", name, strings.Join(c.Models, ", ")))
		return
	}
	model := b.cfg.Current().Catalog.Models[name]
	if model == nil {
		httpErr(w, 503, "model "+name+" is unavailable")
		return
	}
	up, ok := b.cfg.Current().Env.LLM.Upstreams[model.Upstream]
	if !ok {
		httpErr(w, 500, "model "+name+" has no upstream")
		return
	}
	graced := false
	withinBudget := func() bool {
		msg, soft, err := b.overBudget(ctx, c)
		if err != nil {
			slog.Warn("budget lookup", "run", c.Run, "err", err)
			w.Header().Set("Retry-After", "3")
			limitErr(w, 503, "budget_unavailable", "budget accounting unavailable; retry shortly")
			return false
		}
		if msg != "" {
			if soft && (graced || b.useGrace(c)) {
				graced = true
				return true
			}
			limitErr(w, 429, flow.LimitBudgetExceeded, msg)
			return false
		}
		return true
	}
	if !withinBudget() {
		return
	}

	// max_concurrency caps simultaneous calls per model across every pod (a
	// local GPU server, parallel branches). Extra calls wait for a slot, and
	// check the budget again once they have one: calls that finished while
	// this one queued may have used it up.
	if slots := b.modelSlots(name, model.MaxConcurrency); slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return
		}
		if !withinBudget() {
			return
		}
	}

	if up.IsAntigravity() {
		b.llmAntigravity(w, ctx, c, name, model, up, body)
		return
	}

	body["model"] = model.Model
	stream, _ := body["stream"].(bool)
	if stream && path == "/chat/completions" {
		opts, _ := body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}
	out, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(up.BaseURL, "/")+path, bytes.NewReader(out))
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// One session per visit keeps a step's calls on the same upstream prompt
	// cache. Without it LiteLLM's ChatGPT bridge picks a random session per
	// call and agent steps got ~10% cache hits instead of ~90%.
	req.Header.Set("X-Litellm-Session-Id", SessionID(c.Run, c.Seq))
	if key := config.Secret(up.APIKeyEnv); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		httpErr(w, 502, "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Retry-After", "X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset-Tokens", "X-Ratelimit-Remaining-Requests"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	headerCost, _ := strconv.ParseFloat(resp.Header.Get("X-Litellm-Response-Cost"), 64)

	if resp.StatusCode >= 300 || !stream {
		data, _ := io.ReadAll(resp.Body)
		w.Write(data)
		if resp.StatusCode < 300 {
			var parsed map[string]any
			if json.Unmarshal(data, &parsed) == nil {
				b.recordUsage(ctx, c, name, model, usageOf(parsed), headerCost)
			}
		}
		return
	}
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	var usage any
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("data: {")) && bytes.Contains(line, []byte(`"usage"`)) {
			var chunk map[string]any
			if json.Unmarshal(line[6:], &chunk) == nil {
				if u := usageOf(chunk); u != nil {
					usage = u
				}
			}
		}
		w.Write(line)
		w.Write([]byte("\n"))
		if len(line) == 0 && flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	b.recordUsage(context.WithoutCancel(ctx), c, name, model, usage, headerCost)
}

func (b *Broker) llmAntigravity(w http.ResponseWriter, ctx context.Context, c *grant.Claims, name string, model *config.Model, up config.Upstream, body map[string]any) {
	rawMsgs, _ := body["messages"].([]any)
	msgs := make([]antigravity.Message, 0, len(rawMsgs))
	for _, item := range rawMsgs {
		m, _ := item.(map[string]any)
		role, _ := m["role"].(string)
		content := extractMessageText(m["content"])
		msgs = append(msgs, antigravity.Message{Role: role, Content: content})
	}
	var schema map[string]any
	if rf, ok := body["response_format"].(map[string]any); ok {
		if js, ok := rf["json_schema"].(map[string]any); ok {
			if sc, ok := js["schema"].(map[string]any); ok {
				schema = sc
			}
		}
	}
	effort, _ := body["reasoning_effort"].(string)
	resp, err := antigravity.Complete(ctx, antigravity.CompleteRequest{
		Binary:     up.Binary,
		Model:      model.Model,
		Effort:     effort,
		Messages:   msgs,
		JSONSchema: schema,
		AuthEnv:    up.AuthEnv,
		AuthFile:   up.AuthFile,
	})
	if err != nil {
		var aerr *antigravity.Error
		if errors.As(err, &aerr) {
			switch {
			case aerr.RateLimit:
				limitErr(w, 429, flow.LimitRateLimited, aerr.Message)
				return
			case aerr.QuotaLimit:
				limitErr(w, 429, flow.LimitQuotaExhausted, aerr.Message)
				return
			case aerr.AuthError:
				httpErr(w, 401, aerr.Message)
				return
			}
		}
		httpErr(w, 502, "antigravity: "+err.Error())
		return
	}

	usage := map[string]any{
		"prompt_tokens":     float64(resp.Usage.PromptTokens),
		"completion_tokens": float64(resp.Usage.CompletionTokens),
		"total_tokens":      float64(resp.Usage.PromptTokens + resp.Usage.CompletionTokens),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(resp.Usage.CachedTokens),
		},
	}
	b.recordUsage(context.WithoutCancel(ctx), c, name, model, usage, 0)

	stream, _ := body["stream"].(bool)
	if !stream {
		writeJSON(w, 200, map[string]any{
			"id":      "agy-" + SessionID(c.Run, c.Seq),
			"object":  "chat.completion",
			"model":   name,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": resp.Content}, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	emit := func(payload any) {
		raw, _ := json.Marshal(payload)
		w.Write([]byte("data: "))
		w.Write(raw)
		w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emit(map[string]any{
		"id":      "agy-" + SessionID(c.Run, c.Seq),
		"object":  "chat.completion.chunk",
		"model":   name,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": resp.Content}, "finish_reason": nil}},
	})
	emit(map[string]any{
		"id":      "agy-" + SessionID(c.Run, c.Seq),
		"object":  "chat.completion.chunk",
		"model":   name,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   usage,
	})
	w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func extractMessageText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func (b *Broker) recordUsage(ctx context.Context, c *grant.Claims, name string, m *config.Model, usage any, headerCost float64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	u, _ := usage.(map[string]any)
	in, out, _ := runner.UsageTokens(map[string]any{"usage": u})
	cached := cachedTokens(u)
	cost := headerCost
	if cost == 0 {
		cost = in*m.InputPer1M/1e6 + out*m.OutputPer1M/1e6
	}
	if err := b.store.AddUsage(ctx, c.Run, c.Seq, name, int64(in), int64(out), int64(cached), cost); err != nil {
		slog.Warn("record usage", "err", err)
	}
}

// SessionID names the upstream session of one visit.
func SessionID(run string, seq int) string { return fmt.Sprintf("ai-flow-%s-%d", run, seq) }

// usageOf finds the usage object of a chat completion or stream chunk, or of
// a Responses API reply or its response.completed event.
func usageOf(m map[string]any) map[string]any {
	if u, ok := m["usage"].(map[string]any); ok {
		return u
	}
	if r, ok := m["response"].(map[string]any); ok {
		if u, ok := r["usage"].(map[string]any); ok {
			return u
		}
	}
	return nil
}

// cachedTokens reads the prompt tokens served from cache: chat completions'
// prompt_tokens_details, the Responses API's input_tokens_details, or
// LiteLLM's Anthropic-style field.
func cachedTokens(u map[string]any) float64 {
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			if v, ok := d["cached_tokens"].(float64); ok {
				return v
			}
		}
	}
	v, _ := u["cache_read_input_tokens"].(float64)
	return v
}

// graceCalls is enough for a step stopped by its run's budget to report what
// it has, so its work is pushed instead of lost.
const graceCalls = 6

// useGrace admits a visit's few calls after its run's budget ran out. The first
// is refused: that is what tells the runner to ask the agent to finish.
func (b *Broker) useGrace(c *grant.Claims) bool {
	b.graceMu.Lock()
	defer b.graceMu.Unlock()
	if b.grace == nil {
		b.grace = map[string]int{}
	}
	key := fmt.Sprintf("%s/%d", c.Run, c.Seq)
	left, told := b.grace[key]
	if !told {
		b.grace[key] = graceCalls
		return false
	}
	b.grace[key] = max(left-1, 0)
	return left > 0
}

// overBudget checks the node's USD limit and the run's and project's budgets.
// soft means the run's own budget ran out: a step under way may still report.
func (b *Broker) overBudget(ctx context.Context, c *grant.Claims) (msg string, soft bool, err error) {
	run, err := b.store.GetRun(ctx, c.Run)
	if err != nil {
		return "", false, err
	}
	v, err := b.store.GetVisit(ctx, c.Run, c.Seq)
	if err != nil {
		return "", false, err
	}
	res, err := b.engine.Resolved(ctx, run)
	if err != nil {
		return "", false, err
	}
	if v.Node != c.Node || res.Nodes[c.Node] == nil {
		return "", false, fmt.Errorf("budget visit/node mismatch")
	}
	if n := res.Nodes[c.Node]; n != nil && n.LLM != nil && n.LLM.Limits != nil && n.LLM.Limits.USD > 0 && v.CostUSD >= n.LLM.Limits.USD {
		return fmt.Sprintf("node budget of $%.2f is used up", n.LLM.Limits.USD), false, nil
	}
	if bd := res.Flow.Spec.Budget; bd != nil && bd.USD > 0 && run.CostUSD >= bd.USD {
		return fmt.Sprintf("flow budget of $%.2f is used up", bd.USD), true, nil
	}
	if p := res.Project; p != nil && p.Spec.Budget != nil && p.Spec.Budget.USDPerRun > 0 && run.CostUSD >= p.Spec.Budget.USDPerRun {
		return fmt.Sprintf("project cap of $%.2f per run is used up", p.Spec.Budget.USDPerRun), true, nil
	}
	if msg, err := b.engine.TokenBudgetExceeded(ctx, run, res); err != nil || msg != "" {
		return msg, true, err
	}
	if p := res.Project; p != nil && p.Spec.Budget != nil && p.Spec.Budget.USDPerMonth > 0 {
		spent, err := b.monthSpend(ctx, p.Metadata.Name)
		if err != nil {
			return "", false, err
		}
		if spent >= p.Spec.Budget.USDPerMonth {
			return fmt.Sprintf("project monthly cap of $%.2f is used up", p.Spec.Budget.USDPerMonth), false, nil
		}
	}
	return "", false, nil
}

func (b *Broker) monthSpend(ctx context.Context, project string) (float64, error) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return b.store.ProjectSpend(ctx, project, start.UnixMilli(), start.AddDate(0, 1, 0).UnixMilli())
}

func limitErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

// modelSlots returns the semaphore for a catalog model, or nil when unlimited.
func (b *Broker) modelSlots(name string, max int) chan struct{} {
	if max <= 0 {
		return nil
	}
	b.slotsMu.Lock()
	defer b.slotsMu.Unlock()
	if b.slots == nil {
		b.slots = map[string]chan struct{}{}
	}
	// A max_concurrency edit takes effect for new calls; calls holding a
	// slot release it into the channel they took it from.
	if b.slots[name] == nil || cap(b.slots[name]) != max {
		b.slots[name] = make(chan struct{}, max)
	}
	return b.slots[name]
}
