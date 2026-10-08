package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/storymap"
	"github.com/mauza/ai-flow/internal/workspace"
)

func (s *Server) productRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/config", s.getConfig)
	api("POST /api/config", s.editConfig)
	api("GET /api/repos", s.listRepos)
	api("POST /api/repos/link", s.linkRepo)

	api("GET /api/projects/{p}/product", s.product)
	api("POST /api/projects/{p}/workspace/pull", s.pull)
	api("POST /api/projects/{p}/workspace/commit", s.commit)
	api("POST /api/projects/{p}/workspace/discard", s.discard)
	api("GET /api/projects/{p}/files", s.readFile)
	api("PUT /api/projects/{p}/files", s.writeFile)
	api("DELETE /api/projects/{p}/files", s.removeFile)

	api("POST /api/projects/{p}/maps", s.createMap)
	api("GET /api/projects/{p}/maps/{map}", s.getMap)
	api("PUT /api/projects/{p}/maps/{map}", s.saveMap)
	api("DELETE /api/projects/{p}/maps/{map}", s.deleteMap)
	api("POST /api/projects/{p}/maps/{map}/tasks", s.saveTasks)
	api("POST /api/projects/{p}/maps/{map}/send", s.sendToFlow)
	api("GET /api/projects/{p}/maps/{map}/metrics", s.mapMetrics)
	api("GET /api/projects/{p}/maps/{map}/chat", s.getMapChat)
	api("POST /api/projects/{p}/maps/{map}/chat", s.askMap)
	api("DELETE /api/projects/{p}/maps/{map}/chat", s.clearMapChat)
	api("POST /api/projects/{p}/maps/{map}/apply", s.applyChanges)

	api("GET /api/runs/{id}/retro", s.getRetro)
	api("POST /api/runs/{id}/retro", s.startRetro)
}

// ---- config ----

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.Cfg.Current()
	catDoc, projDocs := cfg.Docs()
	var cat map[string]any
	if err := yaml.Unmarshal(catDoc, &cat); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	render := func(v any) string {
		b, _ := yaml.Marshal(v)
		return string(b)
	}
	sections := map[string]map[string]string{}
	for _, name := range app.CatalogSections {
		sections[name] = map[string]string{}
		entries, _ := cat[name].(map[string]any)
		for k, v := range entries {
			sections[name][k] = render(v)
		}
	}
	settings := map[string]string{}
	for _, name := range app.CatalogSettings {
		if v, ok := cat[name]; ok {
			settings[name] = render(v)
		}
	}
	projects := map[string]string{}
	for name, d := range projDocs {
		var doc map[string]any
		yaml.Unmarshal(d, &doc)
		projects[name] = render(map[string]any{"spec": doc["spec"]})
	}
	env := cfg.Env
	hosts := []map[string]any{}
	for _, h := range sortedKeys(env.Git.Hosts) {
		g := env.Git.Hosts[h]
		hosts = append(hosts, map[string]any{"host": h, "token_env": g.TokenEnv, "token_set": config.Secret(g.TokenEnv) != ""})
	}
	upstreams := []map[string]string{}
	for _, name := range sortedKeys(env.LLM.Upstreams) {
		upstreams = append(upstreams, map[string]string{"name": name, "base_url": env.LLM.Upstreams[name].BaseURL})
	}
	mcp := sortedKeys(env.MCP.Servers)
	writeJSON(w, 200, map[string]any{
		"sections": sections, "settings": settings, "projects": projects,
		"seeded_at": s.app.SeededAt(r.Context()),
		"env": map[string]any{
			"git_hosts": hosts, "git_author": env.Git.AuthorName + " <" + env.Git.AuthorEmail + ">",
			"github":    map[string]any{"api_url": env.GitHub.APIURL, "token_env": env.GitHub.TokenEnv, "token_set": config.Secret(env.GitHub.TokenEnv) != ""},
			"upstreams": upstreams, "mcp_servers": nonNil(mcp), "metrics_url": env.Metrics.URL,
		},
	})
}

func (s *Server) editConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Edits []app.ConfigEdit `json:"edits"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Edits) == 0 {
		writeErr(w, 400, "no edits")
		return
	}
	warnings, err := s.app.EditConfig(r.Context(), in.Edits...)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"warnings": nonNil(warnings)})
}

// ---- repositories ----

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.app.GitHub.Repos(r.Context())
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cfg := s.app.Cfg.Current()
	linked := map[string]string{} // owner/name → project
	for _, name := range sortedKeys(cfg.Projects) {
		if g := cfg.Catalog.Grants[cfg.Projects[name].Spec.Repo]; g != nil {
			if repo, err := github.ParseRepoURL(g.URL); err == nil && repo.Host == "github.com" {
				key := strings.ToLower(repo.String())
				if _, ok := linked[key]; !ok {
					linked[key] = name
				}
			}
		}
	}
	type row struct {
		github.RepoInfo
		Project string `json:"project,omitempty"`
	}
	out := []row{}
	for _, rp := range repos {
		out = append(out, row{rp, linked[strings.ToLower(rp.FullName)]})
	}
	writeJSON(w, 200, out)
}

func (s *Server) linkRepo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FullName    string `json:"full_name"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name, err := s.app.LinkRepo(r.Context(), in.FullName, in.Name, in.Description)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]string{"project": name})
}

// ---- workspace ----

// source checks out the project's workspace if needed.
func (s *Server) source(w http.ResponseWriter, r *http.Request) (string, workspace.Source, bool) {
	project := r.PathValue("p")
	src, err := s.app.ProductSource(project)
	if err != nil {
		writeErr(w, 404, err.Error())
		return "", src, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if _, err := s.app.Workspaces.Ensure(ctx, project, src); err != nil {
		writeErr(w, 502, "checking out "+src.Repo.String()+": "+err.Error())
		return "", src, false
	}
	return project, src, true
}

func (s *Server) product(w http.ResponseWriter, r *http.Request) {
	project, src, ok := s.source(w, r)
	if !ok {
		return
	}
	st, err := s.app.Workspaces.Status(project)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	work := s.app.Workspaces.WorkDir(project)
	docs := []string{}
	filepath.WalkDir(filepath.Join(work, "product"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(work, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() && rel == storymap.Root {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(rel, ".md") {
			docs = append(docs, rel)
		}
		return nil
	})
	sort.Slice(docs, func(i, j int) bool {
		// README first, then alphabetical.
		if (docs[i] == "product/README.md") != (docs[j] == "product/README.md") {
			return docs[i] == "product/README.md"
		}
		return docs[i] < docs[j]
	})
	maps, err := storymap.List(work)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	p := s.app.Cfg.Current().Projects[project]
	writeJSON(w, 200, map[string]any{
		"project":   map[string]any{"name": project, "description": p.Spec.Description, "repo": src.Repo.String(), "branch": src.Branch},
		"workspace": st, "docs": docs, "maps": maps,
	})
}

func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	project, src, ok := s.source(w, r)
	if !ok {
		return
	}
	res, err := s.app.Workspaces.Pull(r.Context(), project, src)
	if errors.Is(err, workspace.ErrConflict) {
		writeJSON(w, 409, map[string]any{"error": "the remote changed files you have edited; discard your edits to them first", "conflicts": res.Conflicts})
		return
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	project, src, ok := s.source(w, r)
	if !ok {
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	env := s.app.Cfg.Current().Env
	author := map[string]string{"name": env.Git.AuthorName, "email": env.Git.AuthorEmail}
	sha, err := s.app.Workspaces.Commit(r.Context(), project, src, in.Message, author)
	switch {
	case errors.Is(err, github.ErrNotFastForward):
		writeErr(w, 409, "the branch has new commits; pull, then commit again")
	case err != nil:
		writeErr(w, 400, err.Error())
	default:
		writeJSON(w, 200, map[string]string{"sha": sha, "url": "https://github.com/" + src.Repo.String() + "/commit/" + sha})
	}
}

func (s *Server) discard(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.source(w, r)
	if !ok {
		return
	}
	var in struct {
		Paths []string `json:"paths"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.app.Workspaces.Discard(project, in.Paths); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.source(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	b, err := s.app.Workspaces.Read(project, p)
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, 404, "no such file")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"path": p, "content": string(b)})
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.source(w, r)
	if !ok {
		return
	}
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := s.app.Workspaces.Write(project, in.Path, []byte(in.Content)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) removeFile(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.source(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if strings.HasPrefix(p, storymap.Root+"/") && !strings.HasSuffix(p, ".md") {
		writeErr(w, 400, "remove maps and tasks through the map")
		return
	}
	if err := s.app.Workspaces.Remove(project, p); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

// ---- story maps ----

func (s *Server) loadMap(w http.ResponseWriter, r *http.Request) (string, *storymap.Loaded, bool) {
	project, _, ok := s.source(w, r)
	if !ok {
		return "", nil, false
	}
	l, err := storymap.Load(s.app.Workspaces.WorkDir(project), r.PathValue("map"))
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, 404, "no such story map")
		return "", nil, false
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return "", nil, false
	}
	return project, l, true
}

func (s *Server) createMap(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.source(w, r)
	if !ok {
		return
	}
	var in struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := storymap.CheckID("map", in.ID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, err := s.app.Workspaces.Read(project, storymap.MapPath(in.ID)); err == nil {
		writeErr(w, 409, "a story map with that id already exists")
		return
	}
	// A starting skeleton: one phase, one activity, one release.
	m := storymap.Map{
		Title: in.Title, Description: in.Description,
		Personas: []storymap.Persona{{ID: "user", Name: "User"}},
		Journey:  []storymap.Phase{{ID: "start", Title: "Get started", Activities: []storymap.Activity{{ID: "first-use", Title: "First use", Persona: "user"}}}},
		Releases: []storymap.Release{{ID: "mvp", Title: "MVP", Status: "planned"}},
	}
	if err := m.Check(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.writeYAML(w, project, storymap.MapPath(in.ID), m) {
		return
	}
	writeJSON(w, 201, map[string]string{"id": in.ID})
}

func (s *Server) writeYAML(w http.ResponseWriter, project, p string, v any) bool {
	b, err := storymap.Marshal(v)
	if err == nil {
		err = s.app.Workspaces.Write(project, p, b)
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return false
	}
	return true
}

func (s *Server) getMap(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	works, err := s.app.MapWorks(r.Context(), project, l.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	st, _ := s.app.Workspaces.Status(project)
	dirty := []string{}
	if st != nil {
		for _, c := range st.Changes {
			if strings.HasPrefix(c.Path, storymap.Dir(l.ID)+"/") {
				dirty = append(dirty, c.Path)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"map": l, "works": nonNil(works), "dirty": dirty, "measures": storymap.Measures, "statuses": storymap.Statuses})
}

func (s *Server) saveMap(w http.ResponseWriter, r *http.Request) {
	project, _, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in struct {
		Map storymap.Map `json:"map"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := in.Map.Check(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.writeYAML(w, project, storymap.MapPath(r.PathValue("map")), in.Map) {
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteMap(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	if err := s.app.Workspaces.Remove(project, storymap.Dir(l.ID)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// saveTasks writes and deletes user tasks together (a drag can renumber a column).
func (s *Server) saveTasks(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in struct {
		Tasks  []storymap.Task `json:"tasks"`
		Delete []string        `json:"delete"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	for i := range in.Tasks {
		if err := l.Map.CheckTask(&in.Tasks[i]); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	for _, id := range in.Delete {
		if err := storymap.CheckID("task", id); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	for _, t := range in.Tasks {
		if !s.writeYAML(w, project, storymap.TaskPath(l.ID, t.ID), t) {
			return
		}
	}
	for _, id := range in.Delete {
		if err := s.app.Workspaces.Remove(project, storymap.TaskPath(l.ID, id)); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	w.WriteHeader(204)
}

func (s *Server) sendToFlow(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Release string `json:"release"`
		Plan    *bool  `json:"plan"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	plan := in.Plan == nil || *in.Plan
	t, err := s.app.SendToFlow(r.Context(), project, l.ID, in.Kind, in.ID, in.Release, plan)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) mapMetrics(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	works, err := s.app.MapWorks(r.Context(), project, l.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, 200, s.app.MapMetrics(ctx, project, l, works))
}

func (s *Server) getMapChat(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("p")
	msgs, err := s.app.MapChat(r.Context(), project, r.PathValue("map"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, msgs)
}

func (s *Server) askMap(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in struct {
		Message string `json:"message"`
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
	msg, err := s.app.AskMap(ctx, project, l.ID, in.Message)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, msg)
}

func (s *Server) clearMapChat(w http.ResponseWriter, r *http.Request) {
	if err := s.app.ClearMapChat(r.Context(), r.PathValue("p"), r.PathValue("map")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) applyChanges(w http.ResponseWriter, r *http.Request) {
	project, l, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in struct {
		Changes []app.FileChange `json:"changes"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	for _, c := range in.Changes {
		if !strings.HasPrefix(c.Path, "product/") || path.Clean(c.Path) != c.Path {
			writeErr(w, 400, "changes must stay under product/")
			return
		}
	}
	if err := s.app.ApplyChanges(project, in.Changes); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Report what the map looks like now, problems included.
	after, err := storymap.Load(s.app.Workspaces.WorkDir(project), l.ID)
	if err != nil {
		writeErr(w, 400, "applied, but the map no longer loads: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"problems": after.Problems})
}

// ---- retrospectives ----

func (s *Server) getRetro(w http.ResponseWriter, r *http.Request) {
	retro, err := s.app.GetRetro(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"retro": retro})
}

func (s *Server) startRetro(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	retro, err := s.app.StartRetro(r.Context(), r.PathValue("id"), in.Note)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"retro": retro})
}
