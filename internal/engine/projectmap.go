package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mauza/ai-flow/internal/store"
)

// The project map is what an exploration step learned about the repository
// (architecture, modules, commands, conventions, gotchas), kept per project
// so later steps and later runs start from it instead of exploring again. On
// web-games, read-only investigations used up to 2.8M tokens each, and every
// security review re-read the whole account code.
//
// A step updates it by returning outputs.project_map (markdown) and
// outputs.map_commit (the commit it describes); every agent and llm step then
// gets it in its context.

// ProjectMap is the stored map of a project.
type ProjectMap struct {
	Markdown  string `json:"markdown"`
	Commit    string `json:"commit,omitempty"`
	RunID     string `json:"run_id"`
	Node      string `json:"node"`
	UpdatedAt int64  `json:"updated_at"`
}

// projectMapLimit keeps the map a summary, not a copy of the repository.
const projectMapLimit = 24000

func projectMapKey(project string) string { return "knowledge/" + project + "/map" }

// ProjectMap returns the project's map, or nil when there is none.
func (e *Engine) ProjectMap(ctx context.Context, project string) *ProjectMap {
	raw, err := e.store.GetKV(ctx, projectMapKey(project))
	if err != nil || raw == "" {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("project map", "project", project, "err", err)
		}
		return nil
	}
	var m ProjectMap
	if json.Unmarshal([]byte(raw), &m) != nil || strings.TrimSpace(m.Markdown) == "" {
		return nil
	}
	return &m
}

// ResetProjectMap forgets the project's map.
func (e *Engine) ResetProjectMap(ctx context.Context, project string) error {
	return e.store.WriteKV(ctx, nil, []string{projectMapKey(project)})
}

// saveProjectMap stores a step's project_map output.
func (e *Engine) saveProjectMap(ctx context.Context, r *store.Run, v *store.Visit) {
	var out struct {
		Map    string `json:"project_map"`
		Commit string `json:"map_commit"`
	}
	if json.Unmarshal(v.Outputs, &out) != nil || strings.TrimSpace(out.Map) == "" || r.Project == "" {
		return
	}
	md := strings.TrimSpace(out.Map)
	if len(md) > projectMapLimit {
		md = md[:projectMapLimit] + "\n\n… (trimmed)"
	}
	b, _ := json.Marshal(ProjectMap{Markdown: md, Commit: strings.TrimSpace(out.Commit), RunID: r.ID, Node: v.Node, UpdatedAt: store.Now()})
	if err := e.store.SetKV(ctx, projectMapKey(r.Project), string(b)); err != nil {
		slog.Warn("save project map", "run", r.ID, "err", err)
		return
	}
	e.event(ctx, r, v.Node, "project_map", fmt.Sprintf("%s#%d updated the project map (%d bytes)", v.Node, v.Visit, len(md)))
}

// writeProjectMap puts the project map into a step's context.
func writeProjectMap(sb *strings.Builder, m *ProjectMap) {
	if m == nil {
		return
	}
	when := time.UnixMilli(m.UpdatedAt).UTC().Format("2006-01-02 15:04 UTC")
	sb.WriteString("## Project map\n\nWhat earlier exploration of this repository found")
	if m.Commit != "" {
		fmt.Fprintf(sb, ", as of commit %s", m.Commit)
	}
	fmt.Fprintf(sb, " (%s, run %s). Start from it instead of exploring the repository again; it may be out of date where files changed since", when, m.RunID)
	if m.Commit != "" {
		fmt.Fprintf(sb, " (`git diff --stat %s..HEAD`)", m.Commit)
	}
	sb.WriteString(".\n\n")
	sb.WriteString(m.Markdown)
	sb.WriteString("\n\n")
}
