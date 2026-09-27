// Package broker serves the pod-facing API: the only port node pods can reach.
//
//	POST /v1/exchange     pod identity → bundle (+ grant token)
//	POST /v1/result       node result
//	POST /v1/progress     one-line status for the UI
//	POST /v1/transcript   harness transcript → object store
//	POST /v1/mcp/call     call one granted MCP tool
//	     /llm/v1/...      OpenAI-compatible proxy: model allowlist, budgets, key injection
//	     /git/...         git smart-HTTP proxy: repo + branch policy, token injection
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/engine"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/grant"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/mcpx"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/protocol"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/runner"
	"github.com/mauza/ai-flow/internal/store"
)

// PodIdentifier maps a projected service-account token to the visit it runs.
type PodIdentifier interface {
	Identify(ctx context.Context, token string) (runID string, seq int, err error)
}

type Broker struct {
	cfg    *config.Config
	store  *store.Store
	engine *engine.Engine
	signer *grant.Signer
	obj    objstore.Store
	hub    *hub.Hub
	pods   PodIdentifier
	mcp    *mcpx.Pool
	http   *http.Client
}

func New(cfg *config.Config, st *store.Store, e *engine.Engine, s *grant.Signer, obj objstore.Store, h *hub.Hub, pods PodIdentifier, mcp *mcpx.Pool) *Broker {
	return &Broker{cfg: cfg, store: st, engine: e, signer: s, obj: obj, hub: h, pods: pods, mcp: mcp,
		http: &http.Client{Timeout: 0}}
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /v1/exchange", b.exchange)
	mux.HandleFunc("POST /v1/result", b.withGrant(b.result))
	mux.HandleFunc("POST /v1/progress", b.withGrant(b.progress))
	mux.HandleFunc("POST /v1/transcript", b.withGrant(b.transcript))
	mux.HandleFunc("POST /v1/mcp/call", b.withGrant(b.mcpCall))
	mux.HandleFunc("GET /llm/v1/models", b.withGrant(b.llmModels))
	mux.HandleFunc("POST /llm/v1/chat/completions", b.withGrant(b.llmChat))
	mux.HandleFunc("/git/", b.git)
	return mux
}

type grantHandler func(w http.ResponseWriter, r *http.Request, c *grant.Claims)

func (b *Broker) withGrant(h grantHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := b.signer.Verify(tok, "grant")
		if err != nil {
			httpErr(w, 401, "invalid grant: "+err.Error())
			return
		}
		h(w, r, c)
	}
}

// ---- exchange ----

func (b *Broker) exchange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var runID string
	var seq int
	if pt := r.Header.Get("X-Pod-Token"); pt != "" {
		if b.pods == nil {
			httpErr(w, 401, "pod tokens are not accepted in local mode")
			return
		}
		var err error
		runID, seq, err = b.pods.Identify(ctx, pt)
		if err != nil {
			httpErr(w, 401, "pod identity: "+err.Error())
			return
		}
	} else {
		c, err := b.signer.Verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "launch")
		if err != nil {
			httpErr(w, 401, "invalid launch token: "+err.Error())
			return
		}
		runID, seq = c.Run, c.Seq
	}
	bundle, err := b.buildBundle(ctx, runID, seq)
	if err != nil {
		slog.Warn("exchange refused", "run", runID, "seq", seq, "err", err)
		httpErr(w, 403, err.Error())
		return
	}
	writeJSON(w, 200, bundle)
}

func (b *Broker) buildBundle(ctx context.Context, runID string, seq int) (*protocol.Bundle, error) {
	run, err := b.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	if store.RunDone(run.Status) {
		return nil, fmt.Errorf("run %s is %s", runID, run.Status)
	}
	v, err := b.store.GetVisit(ctx, runID, seq)
	if err != nil {
		return nil, fmt.Errorf("visit %d: %w", seq, err)
	}
	if v.Status != store.VisitPending && v.Status != store.VisitRunning {
		return nil, fmt.Errorf("%s#%d is %s", v.Node, v.Visit, v.Status)
	}
	res, err := b.engine.Resolved(ctx, run)
	if err != nil {
		return nil, err
	}
	n := res.Nodes[v.Node]
	if n == nil {
		return nil, fmt.Errorf("node %s not in flow", v.Node)
	}

	claims := grant.Claims{Kind: "grant", Run: run.ID, Seq: v.Seq, Node: n.ID, Branch: run.Branch}
	bundle := &protocol.Bundle{
		RunID: run.ID, Seq: v.Seq, Node: n.ID, Visit: v.Visit, Type: n.Type,
		PodURL:         strings.TrimSuffix(b.cfg.Env.Server.PodURL, "/"),
		Outcomes:       n.Outcomes,
		Outputs:        n.Outputs,
		ResultSchema:   resolve.ResultSchema(n),
		Harness:        n.Harness,
		TimeoutSeconds: int(n.Timeout.Duration.Seconds()),
	}
	tc := b.engine.TemplateContext(ctx, run, res, n)
	if task, ok := tc["task"].(map[string]any); ok {
		bundle.Task = protocol.Task{Title: str(task["title"]), Body: str(task["body"]), URL: str(task["url"]), Identifier: str(task["identifier"])}
	}
	bundle.Prompt, _ = engine.RenderText(n.Prompt, tc)
	bundle.Context = b.engine.ContextMarkdown(ctx, run, res, n, v.Seq)

	// Every pod node may read the flow's repo; writing needs repo:write.
	if g := b.cfg.Catalog.Grants[res.Repo]; g != nil {
		repo, err := github.ParseRepoURL(g.URL)
		if err != nil {
			return nil, err
		}
		claims.Repo = repo.Host + "/" + repo.Owner + "/" + repo.Name
		for _, gr := range n.Grants {
			if gr == res.Repo+":write" {
				claims.Write = true
			}
		}
		bundle.Repo = &protocol.RepoAccess{
			CloneURL:    bundle.PodURL + "/git/" + claims.Repo + ".git",
			Branch:      run.Branch,
			Base:        run.Base,
			Write:       claims.Write,
			AuthorName:  b.cfg.Env.Git.AuthorName,
			AuthorEmail: b.cfg.Env.Git.AuthorEmail,
			IncludeDiff: n.Type == flow.TypeLLM,
		}
	}

	if n.Type == flow.TypeLLM || n.Type == flow.TypeAgent {
		models := append([]string{n.LLM.Model}, n.LLM.Fallbacks...)
		access := &protocol.LLMAccess{BaseURL: bundle.PodURL + "/llm/v1", Config: n.LLM}
		for _, name := range models {
			m := b.cfg.Catalog.Models[name]
			if m == nil {
				continue
			}
			claims.Models = append(claims.Models, name)
			access.Models = append(access.Models, protocol.ModelInfo{Name: name, ContextTokens: m.ContextTokens, Reasoning: m.Reasoning})
		}
		bundle.LLM = access
	}
	if n.Type == flow.TypeAgent {
		bundle.Tools = runner.DefaultAgentTools
		bundle.Skills = map[string]protocol.SkillDir{}
		for _, s := range n.Skills {
			if sk := b.cfg.Catalog.Skills[s]; sk != nil {
				bundle.Skills[s] = protocol.SkillDir{Files: sk.Files}
			}
		}
		for _, gr := range n.Grants {
			name, _, _ := strings.Cut(gr, ":")
			g := b.cfg.Catalog.Grants[name]
			if g == nil || g.Kind != config.GrantMCP {
				continue
			}
			tools, err := b.mcp.Tools(ctx, g.Server)
			if err != nil {
				return nil, fmt.Errorf("mcp server %s: %w", g.Server, err)
			}
			for _, t := range tools {
				if len(g.Tools) > 0 && !contains(g.Tools, t.Name) {
					continue
				}
				claims.MCP = append(claims.MCP, g.Server+"/"+t.Name)
				bundle.MCP = append(bundle.MCP, protocol.MCPTool{Server: g.Server, Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
			}
		}
	}
	if n.Type == flow.TypeCheck {
		bundle.Check = &protocol.CheckSpec{Run: n.Run, ExitCodes: resolve.CheckExitCodes(&n.Node)}
	}

	ttl := n.Timeout.Duration + 30*time.Minute
	bundle.Grant, err = b.signer.Mint(claims, ttl)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"status": store.VisitRunning, "prompt": bundle.Prompt, "progress": "Starting"}
	if v.StartedAt == 0 {
		fields["started_at"] = store.Now()
	}
	if len(claims.Models) > 0 {
		fields["model"] = claims.Models[0]
	}
	b.store.UpdateVisit(ctx, run.ID, v.Seq, fields)
	b.hub.Publish(hub.Event{Type: "visit", ID: run.ID, Seq: v.Seq})
	return bundle, nil
}

// ---- results ----

func (b *Broker) result(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	var res protocol.Result
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&res); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	outputs, _ := json.Marshal(res.Outputs)
	fields := map[string]any{
		"outcome": res.Outcome, "outputs": string(outputs), "summary": res.Summary, "error": res.Error,
		"commit_sha": res.Commit, "log_tail": res.LogTail, "finished_at": store.Now(), "progress": "",
	}
	if res.Error != "" {
		fields["status"] = store.VisitError
	} else {
		fields["status"] = store.VisitSucceeded
	}
	ok, err := b.store.UpdateVisitIf(ctx, c.Run, c.Seq, []string{store.VisitPending, store.VisitRunning}, fields)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if !ok {
		// Already finished (timed out or canceled): accept so the pod exits cleanly.
		writeJSON(w, 200, map[string]any{"accepted": false})
		return
	}
	if res.Diff != nil {
		d, _ := json.Marshal(res.Diff)
		b.store.UpdateRun(ctx, c.Run, map[string]any{"diff": string(d)})
	}
	b.hub.Publish(hub.Event{Type: "visit", ID: c.Run, Seq: c.Seq})
	b.engine.Wake()
	writeJSON(w, 200, map[string]any{"accepted": true})
}

func (b *Broker) progress(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	var p protocol.Progress
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	b.store.UpdateVisitIf(r.Context(), c.Run, c.Seq, []string{store.VisitPending, store.VisitRunning}, map[string]any{"progress": p.Text})
	b.hub.Publish(hub.Event{Type: "progress", ID: c.Run, Seq: c.Seq, Text: p.Text})
	w.WriteHeader(204)
}

func (b *Broker) transcript(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	key := fmt.Sprintf("runs/%s/%03d-%s.jsonl", c.Run, c.Seq, c.Node)
	if err := b.obj.Put(r.Context(), key, data, "application/x-ndjson"); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	b.store.UpdateVisit(r.Context(), c.Run, c.Seq, map[string]any{"transcript_key": key})
	w.WriteHeader(204)
}

func (b *Broker) mcpCall(w http.ResponseWriter, r *http.Request, c *grant.Claims) {
	var call protocol.MCPCall
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&call); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if !c.AllowsMCP(call.Server, call.Tool) {
		httpErr(w, 403, fmt.Sprintf("%s/%s is not granted to this node", call.Server, call.Tool))
		return
	}
	res, err := b.mcp.Call(r.Context(), call.Server, call.Tool, call.Arguments)
	if err != nil {
		httpErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// ---- helpers ----

func httpErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var errBudget = errors.New("budget exceeded")
