// Package server is the UI-facing HTTP API and static UI.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/hub"
	"github.com/mauza/ai-flow/internal/objstore"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/store"
)

type Server struct {
	app    *app.App
	obj    objstore.Store
	ui     fs.FS
	token  string
	extras map[string]http.Handler
}

// Mount adds a route outside the API auth (e.g. a signed webhook). Call before Handler.
func (s *Server) Mount(pattern string, h http.Handler) {
	if s.extras == nil {
		s.extras = map[string]http.Handler{}
	}
	s.extras[pattern] = h
}

func New(a *app.App, obj objstore.Store, ui fs.FS) *Server {
	return &Server{app: a, obj: obj, ui: ui, token: config.Secret(a.Cfg.Env.Server.AuthTokenEnv)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }

	api("GET /api/overview", s.overview)
	api("GET /api/tasks", s.listTasks)
	api("POST /api/tasks", s.createTask)
	api("GET /api/tasks/{id}", s.getTask)
	api("POST /api/tasks/{id}/plan", s.planTask)
	api("DELETE /api/tasks/{id}", s.deleteTask)

	api("GET /api/flows", s.listFlows)
	api("GET /api/flows/{name}", s.getFlow)
	api("GET /api/flows/{name}/versions/{v}", s.getFlowVersion)
	api("POST /api/flows/{name}", s.saveFlow)
	api("GET /api/flows/{name}/chat", s.getChat)
	api("POST /api/flows/{name}/chat", s.chat)
	api("POST /api/flows/{name}/runs", s.startRun)
	api("POST /api/validate", s.validate)

	api("GET /api/runs", s.listRuns)
	api("GET /api/runs/{id}", s.getRun)
	api("POST /api/runs/{id}/cancel", s.cancelRun)
	api("POST /api/runs/{id}/gates/{seq}", s.decideGate)
	api("GET /api/runs/{id}/visits/{seq}/transcript", s.transcript)

	api("GET /api/events", s.events)
	for pattern, h := range s.extras {
		mux.Handle(pattern, h)
	}
	mux.Handle("/", s.static())
	return mux
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if c, err := r.Cookie("ai_flow_token"); got == "" && err == nil {
				got = c.Value
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeErr(w, 401, "unauthorized")
				return
			}
		}
		h(w, r)
	})
}

// ---- overview & catalog ----

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.Cfg
	cat := cfg.Catalog
	type named struct {
		Name string `json:"name"`
		Desc string `json:"description,omitempty"`
		Kind string `json:"kind,omitempty"`
	}
	var projects []map[string]any
	for _, name := range sortedKeys(cfg.Projects) {
		p := cfg.Projects[name]
		m := map[string]any{"name": name, "description": p.Spec.Description, "repo": p.Spec.Repo, "base": p.Spec.Base, "start": p.Spec.Start, "guidance": p.Spec.Planner.Guidance}
		if p.Spec.Linear != nil {
			m["linear"] = map[string]any{"team": p.Spec.Linear.Team, "label": p.Spec.Linear.Trigger.Label, "states": p.Spec.Linear.Trigger.States}
		}
		projects = append(projects, m)
	}
	var grants []named
	for _, name := range sortedKeys(cat.Grants) {
		g := cat.Grants[name]
		grants = append(grants, named{name, g.Description, g.Kind})
	}
	var skills []named
	for _, name := range sortedKeys(cat.Skills) {
		skills = append(skills, named{name, cat.Skills[name].Description, ""})
	}
	var presets []map[string]any
	for _, name := range sortedKeys(cat.Presets) {
		p := cat.Presets[name]
		presets = append(presets, map[string]any{"name": name, "type": p.Type, "description": p.Description, "outcomes": p.Outcomes, "min_size": p.MinSize})
	}
	var models []map[string]any
	for _, name := range sortedKeys(cat.Models) {
		m := cat.Models[name]
		models = append(models, map[string]any{"name": name, "model": m.Model, "upstream": m.Upstream, "size": m.Size, "context_tokens": m.ContextTokens, "tool_use": m.ToolUse, "cost": m.Cost, "notes": m.Notes})
	}
	var runtimes []map[string]any
	for _, name := range sortedKeys(cat.Runtimes) {
		runtimes = append(runtimes, map[string]any{"name": name, "image": cat.Runtimes[name].Image, "description": cat.Runtimes[name].Description})
	}
	writeJSON(w, 200, map[string]any{
		"projects": projects, "models": models, "runtimes": runtimes, "grants": grants, "skills": skills, "presets": presets,
		"planner": map[string]any{"model": cat.Planner.Model, "guidance": cat.Planner.Guidance},
		"actions": cat.Actions, "node_types": flow.NodeTypes,
		"linear":  cfg.Env.Linear.Enabled, "local": cfg.Env.Runs.Local,
	})
}

// ---- tasks ----

type taskView struct {
	*store.Task
	Planning bool        `json:"planning"`
	Runs     []*store.Run `json:"runs,omitempty"`
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.app.Store.ListTasks(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	runs, _ := s.app.Store.ListRuns(r.Context(), store.RunFilter{Limit: 500})
	byTask := map[string][]*store.Run{}
	for _, run := range runs {
		if run.TaskID != "" && len(byTask[run.TaskID]) < 3 {
			byTask[run.TaskID] = append(byTask[run.TaskID], run)
		}
	}
	out := []taskView{}
	for _, t := range tasks {
		out = append(out, taskView{Task: t, Planning: s.app.Planning(t.ID), Runs: byTask[t.ID]})
	}
	writeJSON(w, 200, out)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		Project string `json:"project"`
		Plan    *bool  `json:"plan"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	t := &store.Task{Source: "manual", Title: strings.TrimSpace(in.Title), Body: in.Body, Project: in.Project}
	plan := in.Plan == nil || *in.Plan
	if err := s.app.CreateTask(r.Context(), t, plan); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.app.Store.GetTask(ctx, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	runs, _ := s.app.Store.ListRuns(ctx, store.RunFilter{TaskID: t.ID, Limit: 50})
	events, _ := s.app.Store.Events(ctx, "task_id", t.ID, 100)
	writeJSON(w, 200, map[string]any{"task": taskView{Task: t, Planning: s.app.Planning(t.ID)}, "runs": nonNil(runs), "events": nonNil(events)})
}

func (s *Server) planTask(w http.ResponseWriter, r *http.Request) {
	t, err := s.app.Store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	s.app.PlanTask(t.ID)
	writeJSON(w, 202, map[string]any{"planning": true})
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Store.DeleteTask(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.app.Hub.Publish(hub.Event{Type: "task", ID: r.PathValue("id")})
	w.WriteHeader(204)
}

// ---- flows ----

func (s *Server) listFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := s.app.Store.ListFlows(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(flows))
}

type flowView struct {
	Flow     *store.FlowVersion   `json:"flow"`
	Versions []*store.FlowVersion `json:"versions"`
	Graph    *resolve.Graph       `json:"graph"`
	Issues   []resolve.Issue      `json:"issues"`
	Runs     []*store.Run         `json:"runs"`
	Task     *store.Task          `json:"task,omitempty"`
	Planning bool                 `json:"planning"`
}

func (s *Server) flowView(ctx context.Context, name string, version int) (*flowView, error) {
	fv, err := s.app.Store.GetFlow(ctx, name, version)
	if err != nil {
		return nil, err
	}
	versions, _ := s.app.Store.FlowVersions(ctx, name)
	runs, _ := s.app.Store.ListRuns(ctx, store.RunFilter{FlowName: name, Limit: 50})
	v := &flowView{Flow: fv, Versions: nonNil(versions), Runs: nonNil(runs)}
	v.Graph, v.Issues = s.analyze(fv.YAML)
	if fv.TaskID != "" {
		v.Task, _ = s.app.Store.GetTask(ctx, fv.TaskID)
		v.Planning = s.app.Planning(fv.TaskID)
	}
	return v, nil
}

func (s *Server) analyze(src string) (*resolve.Graph, []resolve.Issue) {
	f, err := flow.Parse([]byte(src))
	if err != nil {
		return &resolve.Graph{Nodes: []resolve.GraphNode{}, Edges: []resolve.GraphEdge{}}, []resolve.Issue{{Severity: resolve.Error, Message: err.Error()}}
	}
	res := resolve.Resolve(f, s.app.Cfg)
	issues := resolve.Validate(res, s.app.Cfg)
	if issues == nil {
		issues = []resolve.Issue{}
	}
	return res.Graph(), issues
}

func (s *Server) getFlow(w http.ResponseWriter, r *http.Request) {
	v, err := s.flowView(r.Context(), r.PathValue("name"), 0)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) getFlowVersion(w http.ResponseWriter, r *http.Request) {
	ver, _ := strconv.Atoi(r.PathValue("v"))
	v, err := s.flowView(r.Context(), r.PathValue("name"), ver)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) saveFlow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		YAML string `json:"yaml"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	fv, issues, err := s.app.SaveFlow(r.Context(), r.PathValue("name"), in.YAML, "ui", in.Note)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"flow": fv, "issues": nonNil(issues)})
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		YAML string `json:"yaml"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	g, issues := s.analyze(in.YAML)
	writeJSON(w, 200, map[string]any{"graph": g, "issues": issues})
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.app.Store.Chat(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(msgs))
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message string `json:"message"`
		YAML    string `json:"yaml"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Message) == "" {
		writeErr(w, 400, "message is empty")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	res, err := s.app.Revise(ctx, r.PathValue("name"), in.YAML, in.Message)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	g, issues := s.analyze(res.YAML)
	writeJSON(w, 200, map[string]any{"yaml": res.YAML, "explanation": res.Explanation, "valid": res.Valid, "issues": issues, "graph": g, "attempts": res.Attempts})
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version int `json:"version"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	run, err := s.app.Engine.CreateRun(r.Context(), r.PathValue("name"), in.Version, "")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, run)
}

// ---- runs ----

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runs, err := s.app.Store.ListRuns(r.Context(), store.RunFilter{FlowName: q.Get("flow"), TaskID: q.Get("task"), Limit: 200})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(runs))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.app.Store.GetRun(ctx, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	visits, _ := s.app.Store.Visits(ctx, run.ID)
	events, _ := s.app.Store.Events(ctx, "run_id", run.ID, 300)
	fv, _ := s.app.Store.GetFlow(ctx, run.FlowName, run.FlowVersion)
	out := map[string]any{"run": run, "visits": nonNil(visits), "events": nonNil(events)}
	if fv != nil {
		out["graph"], _ = s.analyze(fv.YAML)
		out["yaml"] = fv.YAML
	}
	if run.TaskID != "" {
		if t, err := s.app.Store.GetTask(ctx, run.TaskID); err == nil {
			out["task"] = t
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Engine.Cancel(r.Context(), r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) decideGate(w http.ResponseWriter, r *http.Request) {
	seq, _ := strconv.Atoi(r.PathValue("seq"))
	var in struct {
		Outcome string `json:"outcome"`
		By      string `json:"by"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.By == "" {
		in.By = "ui"
	}
	if err := s.app.Engine.Decide(r.Context(), r.PathValue("id"), seq, in.Outcome, in.By); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.app.Engine.Wake()
	w.WriteHeader(204)
}

func (s *Server) transcript(w http.ResponseWriter, r *http.Request) {
	seq, _ := strconv.Atoi(r.PathValue("seq"))
	v, err := s.app.Store.GetVisit(r.Context(), r.PathValue("id"), seq)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if v.TranscriptKey == "" {
		writeErr(w, 404, "no transcript for this step")
		return
	}
	data, err := s.obj.Get(r.Context(), v.TranscriptKey)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Write(data)
}

// ---- events (SSE) ----

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.app.Hub.Subscribe()
	defer cancel()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

// ---- static UI ----

func (s *Server) static() http.Handler {
	files := http.FileServer(http.FS(s.ui))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log in by opening any page with ?token=<token>: it becomes a cookie.
		if tok := r.URL.Query().Get("token"); s.token != "" && tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: "ai_flow_token", Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
					Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: 90 * 24 * 3600})
			}
			q := r.URL.Query()
			q.Del("token")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if f, err := s.ui.Open(path); err == nil {
				f.Close()
				if strings.HasPrefix(path, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback
		b, err := fs.ReadFile(s.ui, "index.html")
		if err != nil {
			http.Error(w, "UI not built", 404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
}

// ---- helpers ----

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "not found")
		return
	}
	writeErr(w, 500, err.Error())
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
