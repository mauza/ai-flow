package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

func TestOverviewPresetDefinitions(t *testing.T) {
	s := testServer(t, "")
	// Existing catalogs without new metadata remain usable; requires is [] in
	// the overview rather than null, and definition stays a plain flow.Node.
	s.app.Cfg.Catalog.Presets["legacy"] = &config.Preset{Node: flow.Node{Type: flow.TypeGate, Prompt: "Proceed?", Outcomes: []string{"yes", "no"}}}
	rec := do(s.Handler(), "GET", "/api/overview", nil)
	if rec.Code != 200 {
		t.Fatalf("overview: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Presets []map[string]json.RawMessage `json:"presets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range out.Presets {
		var name string
		if err := json.Unmarshal(entry["name"], &name); err != nil {
			t.Fatal(err)
		}
		p := s.app.Cfg.Catalog.Presets[name]
		if p == nil || seen[name] {
			t.Fatalf("unknown or duplicate preset %q", name)
		}
		seen[name] = true
		want := map[string]any{
			"name": name, "type": p.Type, "description": p.Description, "outcomes": p.Outcomes, "min_size": p.MinSize,
			"category": p.Category, "when_to_use": p.WhenToUse, "requires": nonNil(p.Requires), "outputs": p.Outputs, "definition": p.Node,
		}
		for field, value := range want {
			got, ok := entry[field]
			if !ok {
				t.Errorf("%s missing field %s", name, field)
				continue
			}
			expected, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expected, &b); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s.%s got %s, want %s", name, field, got, expected)
			}
		}
		var definition map[string]any
		if err := json.Unmarshal(entry["definition"], &definition); err != nil {
			t.Fatal(err)
		}
		for _, metadata := range []string{"category", "when_to_use", "requires", "min_size", "prompt_file"} {
			if _, ok := definition[metadata]; ok {
				t.Errorf("discovery metadata %s leaked into flow.Node", metadata)
			}
		}
	}
	if len(seen) != len(s.app.Cfg.Catalog.Presets) {
		t.Fatal("overview omitted presets")
	}
}
