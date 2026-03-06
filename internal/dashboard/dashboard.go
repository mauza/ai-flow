package dashboard

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mauza/ai-flow/internal/store"
)


// Dashboard serves the web UI and API endpoints.
type Dashboard struct {
	registry *Registry
	store    *store.Store
	mux      *http.ServeMux
	webFS    fs.FS
}

// New creates a Dashboard. webFS should be the embedded dist filesystem.
func New(registry *Registry, store *store.Store, webFS fs.FS) *Dashboard {
	d := &Dashboard{
		registry: registry,
		store:    store,
		webFS:    webFS,
	}
	d.registerRoutes()
	return d
}

func (d *Dashboard) registerRoutes() {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /dashboard/api/sessions", d.handleListSessions)
	mux.HandleFunc("GET /dashboard/api/sessions/{id}", d.handleGetSession)
	mux.HandleFunc("GET /dashboard/api/sessions/{id}/stream", d.handleStreamSession)
	mux.HandleFunc("DELETE /dashboard/api/sessions/{id}", d.handleKillSession)
	mux.HandleFunc("GET /dashboard/api/runs", d.handleListRuns)
	mux.HandleFunc("GET /dashboard/api/runs/{id}", d.handleGetRun)

	// Static assets from Vite build
	mux.Handle("GET /dashboard/assets/",
		http.StripPrefix("/dashboard/", http.FileServerFS(d.webFS)))

	// SPA catch-all: serve index.html for everything else under /dashboard/
	mux.HandleFunc("/dashboard/", d.handleSPA)

	d.mux = mux
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Redirect bare /dashboard to /dashboard/
	if r.URL.Path == "/dashboard" {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
		return
	}
	d.mux.ServeHTTP(w, r)
}

// --- SPA ---

func (d *Dashboard) handleSPA(w http.ResponseWriter, r *http.Request) {
	content, err := fs.ReadFile(d.webFS, "index.html")
	if err != nil {
		http.Error(w, "dashboard not built (run make build-web)", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

// --- Session API ---

func (d *Dashboard) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	sessions := d.registry.List()
	summaries := make([]SessionSummary, 0, len(sessions))
	for _, s := range sessions {
		summaries = append(summaries, SessionSummary{
			RunID:           s.RunID,
			IssueID:         s.IssueID,
			IssueIdentifier: s.IssueIdentifier,
			IssueTitle:      s.IssueTitle,
			IssueURL:        s.IssueURL,
			StageName:       s.StageName,
			StartedAt:       s.StartedAt,
		})
	}
	writeJSON(w, summaries)
}

func (d *Dashboard) handleGetSession(w http.ResponseWriter, r *http.Request) {
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	s := d.registry.Get(runID)
	if s == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	output := make([]OutputEvent, len(s.buf))
	copy(output, s.buf)
	s.mu.Unlock()

	writeJSON(w, SessionDetail{
		SessionSummary: SessionSummary{
			RunID:           s.RunID,
			IssueID:         s.IssueID,
			IssueIdentifier: s.IssueIdentifier,
			IssueTitle:      s.IssueTitle,
			IssueURL:        s.IssueURL,
			StageName:       s.StageName,
			StartedAt:       s.StartedAt,
		},
		Output: output,
	})
}

func (d *Dashboard) handleKillSession(w http.ResponseWriter, r *http.Request) {
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	if !d.registry.Kill(runID) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	slog.Info("session killed via dashboard", "runID", runID)
	w.WriteHeader(http.StatusNoContent)
}

// handleStreamSession streams live subprocess output via Server-Sent Events.
func (d *Dashboard) handleStreamSession(w http.ResponseWriter, r *http.Request) {
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}

	snapshot, ch, session, found := d.registry.Subscribe(runID)
	if !found {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	defer d.registry.Unsubscribe(runID, ch)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send accumulated buffer as the initial event
	if len(snapshot) > 0 {
		if data, err := json.Marshal(snapshot); err == nil {
			fmt.Fprintf(w, "event: init\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}

	// Stream new events
	for {
		select {
		case evt := <-ch:
			if data, err := json.Marshal(evt); err == nil {
				fmt.Fprintf(w, "event: output\ndata: %s\n\n", data)
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		case <-session.Done:
			// Drain remaining buffered events
		drain:
			for {
				select {
				case evt := <-ch:
					if data, err := json.Marshal(evt); err == nil {
						fmt.Fprintf(w, "event: output\ndata: %s\n\n", data)
						flusher.Flush()
					}
				default:
					break drain
				}
			}
			fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			return
		}
	}
}

// --- Runs API ---

func (d *Dashboard) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	runs, err := d.store.ListRecentRuns(50)
	if err != nil {
		slog.Error("listing recent runs", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Omit output from list to keep payload small
	type runSummary struct {
		ID         int64      `json:"id"`
		IssueID    string     `json:"issue_id"`
		StageName  string     `json:"stage_name"`
		Status     string     `json:"status"`
		ExitCode   *int       `json:"exit_code"`
		PRURL      string     `json:"pr_url"`
		BranchName string     `json:"branch_name"`
		Error      string     `json:"error"`
		StartedAt  any        `json:"started_at"`
		EndedAt    any        `json:"ended_at"`
	}
	summaries := make([]runSummary, 0, len(runs))
	for _, r := range runs {
		s := runSummary{
			ID:         r.ID,
			IssueID:    r.IssueID,
			StageName:  r.StageName,
			Status:     r.Status,
			ExitCode:   r.ExitCode,
			PRURL:      r.PRURL,
			BranchName: r.BranchName,
			Error:      r.Error,
			StartedAt:  r.StartedAt,
			EndedAt:    r.EndedAt,
		}
		summaries = append(summaries, s)
	}
	writeJSON(w, summaries)
}

func (d *Dashboard) handleGetRun(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	run, err := d.store.GetRun(id)
	if err != nil {
		slog.Error("getting run", "id", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	writeJSON(w, run)
}

// --- helpers ---

func parseRunID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return runID, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encoding JSON response", "error", err)
	}
}

