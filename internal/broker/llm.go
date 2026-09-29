package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/grant"
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
	model := b.cfg.Catalog.Models[name]
	if model == nil {
		httpErr(w, 503, "model "+name+" is unavailable")
		return
	}
	up, ok := b.cfg.Env.LLM.Upstreams[model.Upstream]
	if !ok {
		httpErr(w, 500, "model "+name+" has no upstream")
		return
	}
	msg, err := b.overBudget(ctx, c)
	if err != nil {
		slog.Warn("budget lookup", "run", c.Run, "err", err)
		w.Header().Set("Retry-After", "3")
		limitErr(w, 503, "budget_unavailable", "budget accounting unavailable; retry shortly")
		return
	}
	if msg != "" {
		limitErr(w, 429, flow.LimitBudgetExceeded, msg)
		return
	}

	body["model"] = model.Model
	stream, _ := body["stream"].(bool)
	if stream {
		opts, _ := body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}
	out, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(up.BaseURL, "/")+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
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
				b.recordUsage(ctx, c, name, model, parsed["usage"], headerCost)
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
			if json.Unmarshal(line[6:], &chunk) == nil && chunk["usage"] != nil {
				usage = chunk["usage"]
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

func (b *Broker) recordUsage(ctx context.Context, c *grant.Claims, name string, m *config.Model, usage any, headerCost float64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	u, _ := usage.(map[string]any)
	in, _ := u["prompt_tokens"].(float64)
	out, _ := u["completion_tokens"].(float64)
	cost := headerCost
	if cost == 0 {
		cost = in*m.InputPer1M/1e6 + out*m.OutputPer1M/1e6
	}
	if err := b.store.AddUsage(ctx, c.Run, c.Seq, name, int64(in), int64(out), cost); err != nil {
		slog.Warn("record usage", "err", err)
	}
}

// overBudget checks the node's USD limit, the flow budget and the project's per-run cap.
func (b *Broker) overBudget(ctx context.Context, c *grant.Claims) (string, error) {
	run, err := b.store.GetRun(ctx, c.Run)
	if err != nil {
		return "", err
	}
	v, err := b.store.GetVisit(ctx, c.Run, c.Seq)
	if err != nil {
		return "", err
	}
	res, err := b.engine.Resolved(ctx, run)
	if err != nil {
		return "", err
	}
	if v.Node != c.Node || res.Nodes[c.Node] == nil {
		return "", fmt.Errorf("budget visit/node mismatch")
	}
	if n := res.Nodes[c.Node]; n != nil && n.LLM != nil && n.LLM.Limits != nil && n.LLM.Limits.USD > 0 && v.CostUSD >= n.LLM.Limits.USD {
		return fmt.Sprintf("node budget of $%.2f is used up", n.LLM.Limits.USD), nil
	}
	if bd := res.Flow.Spec.Budget; bd != nil && bd.USD > 0 && run.CostUSD >= bd.USD {
		return fmt.Sprintf("flow budget of $%.2f is used up", bd.USD), nil
	}
	if p := res.Project; p != nil && p.Spec.Budget != nil && p.Spec.Budget.USDPerRun > 0 && run.CostUSD >= p.Spec.Budget.USDPerRun {
		return fmt.Sprintf("project cap of $%.2f per run is used up", p.Spec.Budget.USDPerRun), nil
	}
	if p := res.Project; p != nil && p.Spec.Budget != nil && p.Spec.Budget.USDPerMonth > 0 {
		spent, err := b.monthSpend(ctx, p.Metadata.Name)
		if err != nil {
			return "", err
		}
		if spent >= p.Spec.Budget.USDPerMonth {
			return fmt.Sprintf("project monthly cap of $%.2f is used up", p.Spec.Budget.USDPerMonth), nil
		}
	}
	return "", nil
}

func (b *Broker) monthSpend(ctx context.Context, project string) (float64, error) {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return b.store.ProjectSpend(ctx, project, start.UnixMilli(), start.AddDate(0, 1, 0).UnixMilli())
}

func limitErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}
