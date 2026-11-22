package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/mauza/ai-flow/internal/app"
	"github.com/mauza/ai-flow/internal/store"
)

func (s *Server) improvementRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/runs/{id}/grades", s.getRunGrades)
	api("POST /api/runs/{id}/grade", s.gradeRun)

	api("GET /api/improvement/summary", s.improvementSummary)
	api("GET /api/improvement/grades", s.listImprovementGrades)
	api("POST /api/improvement/grade-ungraded", s.gradeUngradedRuns)
	api("POST /api/improvement/grades/{id}/disagree", s.disagreeGrade)

	api("GET /api/improvement/proposals", s.listProposals)
	api("POST /api/improvement/run", s.runImprover)
	api("POST /api/improvement/proposals/{id}/apply", s.applyProposal)
	api("POST /api/improvement/proposals/{id}/reject", s.rejectProposal)

	api("GET /api/improvement/revisions", s.listPromptRevisions)
}

func (s *Server) getRunGrades(w http.ResponseWriter, r *http.Request) {
	v, err := s.app.GetRunGrades(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) gradeRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.app.Store.GetRun(r.Context(), id); err != nil {
		writeStoreErr(w, err)
		return
	}
	if r.URL.Query().Get("sync") == "1" {
		grades, err := s.app.GradeRunSync(r.Context(), id)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"run_id": id, "grades": nonNil(grades)})
		return
	}
	started := s.app.StartGradeRun(id)
	writeJSON(w, 202, map[string]any{"run_id": id, "started": started, "grading": true})
}

func (s *Server) improvementSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.app.ImprovementSummary(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, sum)
}

func (s *Server) listImprovementGrades(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 200
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}
	f := store.GradeFilter{
		RunID:   q.Get("run"),
		Project: q.Get("project"),
		Rubric:  q.Get("rubric"),
		Blame:   q.Get("blame"),
		Limit:   limit,
	}
	if p := q.Get("pass"); p == "true" || p == "1" {
		v := true
		f.Pass = &v
	} else if p == "false" || p == "0" {
		v := false
		f.Pass = &v
	}
	if q.Get("unreviewed") == "true" || q.Get("unreviewed") == "1" {
		f.Unreviewed = true
	}
	grades, err := s.app.Store.ListRunGrades(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(grades))
}

func (s *Server) gradeUngradedRuns(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Project string `json:"project"`
		Limit   int    `json:"limit"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	queued, err := s.app.GradeUngradedRuns(r.Context(), in.Project, in.Limit)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"queued": nonNil(queued)})
}

func (s *Server) disagreeGrade(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disagreed bool   `json:"disagreed"`
		By        string `json:"by"`
		Note      string `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	g, err := s.app.DisagreeGrade(r.Context(), r.PathValue("id"), in.Disagreed, in.By, in.Note)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) listProposals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	props, err := s.app.Store.ListProposals(r.Context(), store.ProposalFilter{
		Status:     q.Get("status"),
		TargetKind: q.Get("target_kind"),
		Target:     q.Get("target"),
		Limit:      limit,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(props))
}

func (s *Server) runImprover(w http.ResponseWriter, r *http.Request) {
	var in app.ImproverOptions
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	in.Trigger = "manual"
	res, err := s.app.RunImprover(r.Context(), in)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) applyProposal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		By        string `json:"by"`
		Note      string `json:"note"`
		AfterText string `json:"after_text"`
		Force     bool   `json:"force"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	prop, warnings, err := s.app.ApplyProposal(r.Context(), r.PathValue("id"), in.By, in.Note, in.AfterText, in.Force)
	var affected *app.RunsAffectedError
	var conflict *app.ProposalConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "not found")
		return
	case errors.As(err, &affected):
		writeJSON(w, 409, map[string]any{"error": err.Error(), "runs": affected.Runs})
		return
	case errors.As(err, &conflict):
		writeJSON(w, 409, map[string]any{"error": err.Error(), "conflict": true})
		return
	case err != nil:
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"proposal": prop, "warnings": nonNil(warnings)})
}

func (s *Server) rejectProposal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		By   string `json:"by"`
		Note string `json:"note"`
	}
	if r.ContentLength > 0 && !readJSON(w, r, &in) {
		return
	}
	prop, err := s.app.RejectProposal(r.Context(), r.PathValue("id"), in.By, in.Note)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "not found")
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"proposal": prop})
}

func (s *Server) listPromptRevisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	revs, err := s.app.Store.ListPromptRevisions(r.Context(), q.Get("target_kind"), q.Get("target"), limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, nonNil(revs))
}
