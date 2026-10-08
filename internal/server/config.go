package server

import (
	"errors"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/github"
)

// configRoutes serves the editable catalog and projects, and repository linking.
func (s *Server) configRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/config", s.getConfig)
	api("POST /api/config", s.editConfig)
	api("GET /api/repos", s.listRepos)
	api("POST /api/repos/link", s.linkRepo)
}

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
		"seeded_at": s.app.SeededAt(r.Context()), "files_error": s.app.FilesError(r.Context()),
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
		Force bool             `json:"force"` // apply even if active runs would fail with drift
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Edits) == 0 {
		writeErr(w, 400, "no edits")
		return
	}
	warnings, err := s.app.EditConfig(r.Context(), in.Force, in.Edits...)
	var affected *app.RunsAffectedError
	switch {
	case errors.As(err, &affected):
		writeJSON(w, 409, map[string]any{"error": err.Error(), "runs": affected.Runs})
		return
	case err != nil:
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
