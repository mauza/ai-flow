package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mauza/ai-flow/internal/llm"
	"github.com/mauza/ai-flow/internal/store"
	"github.com/mauza/ai-flow/internal/storymap"
	"github.com/mauza/ai-flow/internal/workspace"
)

// FileChange is a proposed edit to a product file: nil Content deletes it.
type FileChange struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}

// MapMessage is one turn of a map's assistant chat.
type MapMessage struct {
	Role    string       `json:"role"` // user | assistant
	Content string       `json:"content"`
	Changes []FileChange `json:"changes,omitempty"`
	Issues  []string     `json:"issues,omitempty"` // why proposed changes could not be applied as-is
	At      int64        `json:"at"`
}

func kvMapChat(project, mapID string) string { return path.Join("storymap/chat", project, mapID) }

// MapChat returns a map's assistant conversation.
func (a *App) MapChat(ctx context.Context, project, mapID string) ([]MapMessage, error) {
	raw, err := a.Store.GetKV(ctx, kvMapChat(project, mapID))
	if errors.Is(err, store.ErrNotFound) {
		return []MapMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []MapMessage
	return out, json.Unmarshal([]byte(raw), &out)
}

// ClearMapChat forgets a map's conversation.
func (a *App) ClearMapChat(ctx context.Context, project, mapID string) error {
	return a.Store.WriteKV(ctx, nil, []string{kvMapChat(project, mapID)})
}

const mapChatSystem = `You help a product owner work on a user story map that lives as YAML files in their repository. You answer questions about the map and, when asked to change it, propose edits to its files. Proposed edits are shown to the user, who applies or ignores them; nothing is committed until they choose to.

Story map files (paths are relative to the repository root):
- %[1]s/map.yaml:
    title, description
    personas: [{id, name, description}]
    journey: phases left to right: [{id, title, description, activities: [{id, title, description, persona}]}]
    releases: slices top to bottom: [{id, title, goal, status: planned|in_progress|released, date}]
    metrics: [{id, title, description, kind: product|delivery, query (product: PromQL), measure (delivery: %[2]s), release, target, direction: up|down, unit}]
- %[1]s/tasks/<task-id>.yaml, one user task per file:
    title, activity (an activity id), release (a release id), order (position within its activity and release), persona, story ("As a <persona> I want <goal> so that <benefit>"), description, acceptance: [criteria], metrics: [metric ids it should move], status: todo|in_progress|done|blocked
Ids are lowercase slugs (a-z, 0-9, -). Use story-mapping vocabulary: the journey's phases hold activities (the backbone); user tasks hang under activities and are sliced into releases. Keep tasks small enough to ship one at a time, and give each testable acceptance criteria.

Reply with one JSON object and nothing else:
{"reply": "<markdown answer for the user>", "changes": [{"path": "<file path>", "content": "<the complete new file content, or null to delete the file>"}]}
Use an empty changes list when no edit is needed. Only change files under product/. Always send complete file contents, never fragments.`

// AskMap sends a message to the map assistant and stores both turns.
func (a *App) AskMap(ctx context.Context, project, mapID, message string) (*MapMessage, error) {
	work := a.Workspaces.WorkDir(project)
	if _, err := storymap.Load(work, mapID); err != nil {
		return nil, err
	}
	history, err := a.MapChat(ctx, project, mapID)
	if err != nil {
		return nil, err
	}
	cfg := a.Cfg.Current()
	model := cfg.Catalog.Planner.Model
	if model == "" {
		return nil, fmt.Errorf("no planner model is configured")
	}
	msgs := []llm.Message{
		{Role: "system", Content: fmt.Sprintf(mapChatSystem, storymap.Dir(mapID), strings.Join(storymap.Measures, ", "))},
		{Role: "user", Content: "Current files:\n\n" + mapFiles(work, mapID)},
		{Role: "assistant", Content: `{"reply": "I have read the map.", "changes": []}`},
	}
	for _, m := range history[max(0, len(history)-12):] {
		content := m.Content
		if m.Role == "assistant" {
			b, _ := json.Marshal(map[string]any{"reply": m.Content, "changes": m.Changes})
			content = string(b)
		}
		msgs = append(msgs, llm.Message{Role: m.Role, Content: content})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: message})

	var answer *MapMessage
	for attempt := 0; attempt < 2; attempt++ {
		reply, _, err := a.LLM.Chat(ctx, model, msgs, llm.Options{Temperature: 0.2, Stream: cfg.Catalog.Planner.Stream})
		if err != nil {
			return nil, err
		}
		answer, err = parseMapReply(reply)
		if err != nil {
			msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
				llm.Message{Role: "user", Content: "That was not the JSON object described. Reply again with only the JSON object."})
			continue
		}
		answer.Issues = a.checkChanges(work, mapID, answer.Changes)
		if len(answer.Issues) == 0 || attempt == 1 {
			break
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: reply},
			llm.Message{Role: "user", Content: "These proposed changes are invalid:\n- " + strings.Join(answer.Issues, "\n- ") + "\nFix them and reply again with the whole JSON object."})
	}
	if answer == nil {
		return nil, fmt.Errorf("the assistant did not reply with the expected JSON")
	}
	now := store.Now()
	answer.At = now
	history = append(history, MapMessage{Role: "user", Content: message, At: now}, *answer)
	b, _ := json.Marshal(history)
	if err := a.Store.SetKV(ctx, kvMapChat(project, mapID), string(b)); err != nil {
		return nil, err
	}
	return answer, nil
}

func parseMapReply(reply string) (*MapMessage, error) {
	s := strings.TrimSpace(reply)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	var out struct {
		Reply   string       `json:"reply"`
		Changes []FileChange `json:"changes"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Reply) == "" && len(out.Changes) == 0 {
		return nil, fmt.Errorf("empty reply")
	}
	return &MapMessage{Role: "assistant", Content: out.Reply, Changes: out.Changes}, nil
}

// checkChanges validates proposed changes against the map as it would be
// after applying them.
func (a *App) checkChanges(work, mapID string, changes []FileChange) []string {
	var issues []string
	tmp, err := os.MkdirTemp("", "ai-flow-map-")
	if err != nil {
		return []string{err.Error()}
	}
	defer os.RemoveAll(tmp)
	if err := os.CopyFS(filepath.Join(tmp, "product"), os.DirFS(filepath.Join(work, "product"))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return []string{err.Error()}
	}
	for _, c := range changes {
		if err := workspace.CheckPath(c.Path); err != nil {
			issues = append(issues, err.Error())
			continue
		}
		p := filepath.Join(tmp, filepath.FromSlash(c.Path))
		if c.Content == nil {
			os.RemoveAll(p)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(*c.Content), 0o644)
	}
	l, err := storymap.Load(tmp, mapID)
	if err != nil {
		return append(issues, err.Error())
	}
	// Report only problems the changes introduce.
	before := map[string]bool{}
	if old, err := storymap.Load(work, mapID); err == nil {
		for _, p := range old.Problems {
			before[p] = true
		}
	}
	for _, p := range l.Problems {
		if !before[p] {
			issues = append(issues, p)
		}
	}
	return issues
}

// ApplyChanges writes proposed changes to the workspace (not committed).
func (a *App) ApplyChanges(project string, changes []FileChange) error {
	for _, c := range changes {
		if err := workspace.CheckPath(c.Path); err != nil {
			return err
		}
	}
	for _, c := range changes {
		var err error
		if c.Content == nil {
			err = a.Workspaces.Remove(project, c.Path)
		} else {
			err = a.Workspaces.Write(project, c.Path, []byte(*c.Content))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mapFiles renders the map, its tasks and the product README for a prompt.
func mapFiles(work, mapID string) string {
	var b strings.Builder
	add := func(rel string, limit int) {
		data, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		s := string(data)
		if len(s) > limit {
			s = s[:limit] + "\n… (truncated)"
		}
		fmt.Fprintf(&b, "--- %s\n%s\n", rel, s)
	}
	add("product/README.md", 12000)
	add(storymap.MapPath(mapID), 40000)
	files, _ := filepath.Glob(filepath.Join(work, filepath.FromSlash(storymap.Dir(mapID)), "tasks", "*.yaml"))
	for _, f := range files {
		add(path.Join(storymap.Dir(mapID), "tasks", filepath.Base(f)), 8000)
	}
	return b.String()
}
