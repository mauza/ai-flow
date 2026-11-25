package server

import (
	"net/http"

	"github.com/mauza/ai-flow/internal/engine"
)

func (s *Server) loopRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/schedules", s.listSchedules)
	api("GET /api/projects/{project}/schedules", s.listProjectSchedules)
	api("POST /api/projects/{project}/schedules/{name}/run", s.triggerSchedule)

	api("GET /api/projects/{project}/ledger", s.getProjectLedger)
	api("POST /api/projects/{project}/ledger", s.upsertProjectLedger)
	api("DELETE /api/projects/{project}/ledger", s.resetProjectLedger)
	api("DELETE /api/projects/{project}/ledger/{key}", s.deleteProjectLedgerEntry)

	api("GET /api/projects/{project}/baselines", s.getProjectBaselines)
	api("PUT /api/projects/{project}/baselines", s.setProjectBaselines)
	api("DELETE /api/projects/{project}/baselines", s.resetProjectBaselines)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.app.ListSchedules(r.Context(), r.URL.Query().Get("project")))
}

func (s *Server) listProjectSchedules(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if s.app.Cfg.Current().Projects[project] == nil {
		writeErr(w, 404, "unknown project")
		return
	}
	writeJSON(w, 200, s.app.ListSchedules(r.Context(), project))
}

func (s *Server) triggerSchedule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Force bool `json:"force"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	st, err := s.app.TriggerSchedule(r.Context(), r.PathValue("project"), r.PathValue("name"), in.Force)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) getProjectLedger(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if s.app.Cfg.Current().Projects[project] == nil {
		writeErr(w, 404, "unknown project")
		return
	}
	entries := s.app.Engine.Ledger(r.Context(), project)
	if entries == nil {
		entries = []engine.LedgerEntry{}
	}
	writeJSON(w, 200, entries)
}

func (s *Server) upsertProjectLedger(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if s.app.Cfg.Current().Projects[project] == nil {
		writeErr(w, 404, "unknown project")
		return
	}
	var entry engine.LedgerEntry
	if !readJSON(w, r, &entry) {
		return
	}
	if err := s.app.Engine.UpsertLedger(r.Context(), project, entry); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, s.app.Engine.Ledger(r.Context(), project))
}

func (s *Server) resetProjectLedger(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.app.Engine.ResetLedger(r.Context(), project); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) deleteProjectLedgerEntry(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.app.Engine.DeleteLedgerEntry(r.Context(), project, r.PathValue("key")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (s *Server) getProjectBaselines(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if s.app.Cfg.Current().Projects[project] == nil {
		writeErr(w, 404, "unknown project")
		return
	}
	writeJSON(w, 200, s.app.Engine.Baselines(r.Context(), project))
}

func (s *Server) setProjectBaselines(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if s.app.Cfg.Current().Projects[project] == nil {
		writeErr(w, 404, "unknown project")
		return
	}
	var updates map[string]engine.BaselineEntry
	if !readJSON(w, r, &updates) {
		return
	}
	if err := s.app.Engine.SetBaselines(r.Context(), project, updates); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, s.app.Engine.Baselines(r.Context(), project))
}

func (s *Server) resetProjectBaselines(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if err := s.app.Engine.ResetBaselines(r.Context(), project); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}
