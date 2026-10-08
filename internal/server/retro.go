package server

import "net/http"

// Run retrospectives: GET /api/runs/{id}/retro, POST to (re)start one.

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
