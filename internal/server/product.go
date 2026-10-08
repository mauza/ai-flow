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

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/github"
	"github.com/mauza/ai-flow/internal/storymap"
	"github.com/mauza/ai-flow/internal/workspace"
)

// productRoutes serves product workspaces, docs and user story maps.
func (s *Server) productRoutes(api func(string, http.HandlerFunc)) {
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
