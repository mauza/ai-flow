package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/protocol"
)

// Shim is a localhost OpenAI-compatible proxy the harness talks to. It
// forwards to the control plane's LLM proxy and applies the node's on_limit
// policy: retry, wait, fall back to the next model, or stop the node.
//
// It is behaviour, not security: budgets and model allowlists are enforced
// again by the control plane.
type Shim struct {
	upstream string
	grant    string
	models   []protocol.ModelInfo
	cfg      *flow.LLMConfig
	client   *http.Client
	sleep    func(context.Context, time.Duration) error

	mu        sync.Mutex
	current   int
	calls     int
	tokensIn  int64
	tokensOut int64
	stopped   *LimitStop

	stop chan struct{}
	srv  *http.Server
	addr string
}

// LimitStop tells the runner to end the node: Outcome "limit" or a failure.
type LimitStop struct {
	Kind    string // rate_limited | quota_exhausted | context_exceeded | budget_exceeded
	Action  string // outcome | fail
	Message string
}

func NewShim(access *protocol.LLMAccess, grant string) *Shim {
	return &Shim{
		upstream: strings.TrimSuffix(access.BaseURL, "/"),
		grant:    grant,
		models:   access.Models,
		cfg:      access.Config,
		client:   &http.Client{Timeout: 30 * time.Minute},
		sleep:    sleepCtx,
		stop:     make(chan struct{}),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start listens on a random localhost port.
func (s *Shim) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]any
		for _, m := range s.models {
			data = append(data, map[string]any{"id": m.Name, "object": "model"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})
	s.srv = &http.Server{Handler: mux}
	s.addr = "http://" + ln.Addr().String() + "/v1"
	go s.srv.Serve(ln)
	return s.addr, nil
}

func (s *Shim) Close() {
	if s.srv != nil {
		s.srv.Close()
	}
}

// Done is closed when a limit ends the node; StopInfo then says why.
func (s *Shim) Done() <-chan struct{} { return s.stop }

func (s *Shim) StopInfo() *LimitStop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// Usage returns total tokens and calls seen by the shim.
func (s *Shim) Usage() (in, out int64, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokensIn, s.tokensOut, s.calls
}

// CurrentModel is the model the shim is currently sending.
func (s *Shim) CurrentModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models[s.current].Name
}

func (s *Shim) signal(st LimitStop) {
	s.mu.Lock()
	if s.stopped != nil {
		s.mu.Unlock()
		return
	}
	s.stopped = &st
	s.mu.Unlock()
	slog.Warn("llm limit ends node", "kind", st.Kind, "action", st.Action, "msg", st.Message)
	close(s.stop)
}

func (s *Shim) onLimit(kind string) *flow.OnLimit {
	if s.cfg != nil && s.cfg.OnLimit != nil {
		if ol, ok := s.cfg.OnLimit[kind]; ok && ol != nil {
			return ol
		}
	}
	return &flow.OnLimit{Action: flow.ActFail}
}

type upstreamErr struct {
	status  int
	body    []byte
	header  http.Header
	kind    string
	message string
}

// Complete sends one chat completion (non-streaming) through the shim logic.
// Used by llm nodes, which don't go through a harness.
func (s *Shim) Complete(ctx context.Context, body map[string]any) (map[string]any, error) {
	body["stream"] = false
	resp, uerr, err := s.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if uerr != nil {
		return nil, fmt.Errorf("llm %s: %s", uerr.kind, uerr.message)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm: bad response: %w", err)
	}
	s.recordUsage(out)
	return out, nil
}

func (s *Shim) handleChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	stream, _ := body["stream"].(bool)
	resp, uerr, err := s.do(r.Context(), body)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]any{"message": err.Error(), "type": "shim_error"}})
		return
	}
	if uerr != nil {
		for k, v := range uerr.header {
			if strings.EqualFold(k, "content-type") {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(uerr.status)
		w.Write(uerr.body)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		if strings.EqualFold(k, "content-length") {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	if !stream {
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if json.Unmarshal(raw, &out) == nil {
			s.recordUsage(out)
		}
		w.Write(raw)
		s.checkBudget()
		return
	}
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("data: {")) {
			var chunk map[string]any
			if json.Unmarshal(line[6:], &chunk) == nil {
				if _, ok := chunk["usage"].(map[string]any); ok {
					s.recordUsage(chunk)
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
	s.checkBudget()
}

// do sends body upstream, applying on_limit until it succeeds, gives up, or
// the node is stopped. A non-nil *upstreamErr is a final error to pass back.
func (s *Shim) do(ctx context.Context, body map[string]any) (*http.Response, *upstreamErr, error) {
	s.mu.Lock()
	if s.stopped != nil {
		st := *s.stopped
		s.mu.Unlock()
		return nil, &upstreamErr{status: 429, body: errBody(st.Kind, "node stopped: "+st.Message), kind: st.Kind, message: st.Message}, nil
	}
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if l := s.limits(); l != nil && l.Turns > 0 && calls > l.Turns {
		return nil, s.budgetStop(fmt.Sprintf("turn limit %d reached", l.Turns)), nil
	}

	attempts := map[string]int{}
	var waited time.Duration
outer:
	for {
		s.mu.Lock()
		model := s.models[s.current].Name
		s.mu.Unlock()
		body["model"] = model
		if stream, _ := body["stream"].(bool); stream {
			opts, _ := body["stream_options"].(map[string]any)
			if opts == nil {
				opts = map[string]any{}
			}
			opts["include_usage"] = true
			body["stream_options"] = opts
		}
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, "POST", s.upstream+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.grant)
		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			// connection-level failure: treat as a transient rate limit
			resp = &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(err.Error())), Header: http.Header{}}
		}
		if resp.StatusCode < 400 {
			return resp, nil, nil
		}
		errRaw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ue := classify(resp.StatusCode, errRaw, resp.Header)
		if ue.kind == "" {
			return nil, ue, nil // not a limit: pass the error through
		}
		slog.Info("llm limit", "kind", ue.kind, "status", ue.status, "model", model, "msg", truncate(ue.message, 200))

		ol := s.onLimit(ue.kind)
		action := ol.Action
		for {
			switch action {
			case flow.ActRetry:
				max := ol.Max
				if max == 0 {
					max = 3
				}
				if attempts[ue.kind] < max {
					attempts[ue.kind]++
					d := retryAfter(ue.header, backoff(ol, attempts[ue.kind]))
					if err := s.sleep(ctx, d); err != nil {
						return nil, nil, err
					}
					continue outer
				}
				action = orFail(ol.Then)
				continue
			case flow.ActWait:
				d := resetIn(ue.header, time.Minute)
				maxWait := ol.MaxWait.Duration
				if maxWait == 0 {
					maxWait = 30 * time.Minute
				}
				if waited+d <= maxWait {
					waited += d
					if err := s.sleep(ctx, d); err != nil {
						return nil, nil, err
					}
					continue outer
				}
				action = orFail(ol.Then)
				continue
			case flow.ActFallback:
				s.mu.Lock()
				ok := s.current+1 < len(s.models)
				if ok {
					s.current++
					slog.Info("llm fallback", "to", s.models[s.current].Name)
				}
				s.mu.Unlock()
				if ok {
					attempts = map[string]int{}
					continue outer
				}
				action = orFail(ol.Then)
				continue
			case flow.ActOutcome:
				s.signal(LimitStop{Kind: ue.kind, Action: flow.ActOutcome, Message: ue.message})
				return nil, ue, nil
			default:
				s.signal(LimitStop{Kind: ue.kind, Action: flow.ActFail, Message: ue.message})
				return nil, ue, nil
			}
		}
	}
}

func orFail(a string) string {
	if a == "" || a == flow.ActRetry || a == flow.ActWait {
		return flow.ActFail
	}
	return a
}

func (s *Shim) limits() *flow.Limits {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.Limits
}

func (s *Shim) budgetStop(msg string) *upstreamErr {
	ol := s.onLimit(flow.LimitBudgetExceeded)
	action := flow.ActFail
	if ol.Action == flow.ActOutcome || ol.Then == flow.ActOutcome {
		action = flow.ActOutcome
	}
	s.signal(LimitStop{Kind: flow.LimitBudgetExceeded, Action: action, Message: msg})
	return &upstreamErr{status: 429, body: errBody(flow.LimitBudgetExceeded, msg), kind: flow.LimitBudgetExceeded, message: msg}
}

func (s *Shim) checkBudget() {
	l := s.limits()
	if l == nil || l.Tokens == 0 {
		return
	}
	in, out, _ := s.Usage()
	if in+out > int64(l.Tokens) {
		s.budgetStop(fmt.Sprintf("token limit %d reached (%d used)", l.Tokens, in+out))
	}
}

func (s *Shim) recordUsage(resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	in, _ := u["prompt_tokens"].(float64)
	out, _ := u["completion_tokens"].(float64)
	s.mu.Lock()
	s.tokensIn += int64(in)
	s.tokensOut += int64(out)
	s.mu.Unlock()
}

func classify(status int, body []byte, h http.Header) *upstreamErr {
	ue := &upstreamErr{status: status, body: body, header: h}
	var parsed struct {
		Error any `json:"error"`
	}
	msg := string(body)
	typ := ""
	if json.Unmarshal(body, &parsed) == nil {
		switch e := parsed.Error.(type) {
		case map[string]any:
			if m, ok := e["message"].(string); ok {
				msg = m
			}
			if t, ok := e["type"].(string); ok {
				typ = t
			}
			if c, ok := e["code"].(string); ok && typ == "" {
				typ = c
			}
		case string:
			msg = e
		}
	}
	ue.message = msg
	low := strings.ToLower(msg + " " + typ)
	switch {
	case typ == flow.LimitBudgetExceeded || strings.Contains(low, "budget has been exceeded") || strings.Contains(low, "budget_exceeded"):
		ue.kind = flow.LimitBudgetExceeded
	case strings.Contains(low, "context length") || strings.Contains(low, "context_length") || strings.Contains(low, "maximum context") ||
		strings.Contains(low, "context window") || strings.Contains(low, "too many tokens") || strings.Contains(low, "exceeds the available context") ||
		strings.Contains(low, "exceed_context"):
		ue.kind = flow.LimitContextExceeded
	case status == 429 && (strings.Contains(low, "quota") || strings.Contains(low, "usage limit") || strings.Contains(low, "insufficient")):
		ue.kind = flow.LimitQuotaExhausted
	case status == 429 || status == 502 || status == 503 || status == 504 || status == 529:
		ue.kind = flow.LimitRateLimited
	}
	return ue
}

func backoff(ol *flow.OnLimit, attempt int) time.Duration {
	b := ol.Backoff.Duration
	if b == 0 {
		b = 10 * time.Second
	}
	d := b * time.Duration(1<<(attempt-1))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func retryAfter(h http.Header, def time.Duration) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 && secs < 3600 {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

// resetIn reads provider reset headers (seconds or RFC3339), else def.
func resetIn(h http.Header, def time.Duration) time.Duration {
	if d := retryAfter(h, 0); d > 0 {
		return d
	}
	for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens", "anthropic-ratelimit-requests-reset", "x-ratelimit-reset"} {
		v := h.Get(k)
		if v == "" {
			continue
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			if secs > 1e9 { // epoch seconds
				return time.Until(time.Unix(int64(secs), 0))
			}
			return time.Duration(secs * float64(time.Second))
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return time.Until(t)
		}
	}
	return def
}

func errBody(typ, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": typ, "message": msg}})
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
