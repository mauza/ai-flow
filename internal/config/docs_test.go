package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
)

// The database keeps the catalog and projects as the documents Docs renders,
// so reading them back must give the same config.
func TestDocsRoundTrip(t *testing.T) {
	for _, dir := range []string{"../../deploy/config", "../../examples/templates/config"} {
		cfg, err := config.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		cat, projects := cfg.Docs()
		var docs [][]byte
		for _, d := range projects {
			docs = append(docs, d)
		}
		next, err := cfg.WithDocs(cat, docs)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for name, p := range cfg.Catalog.Presets {
			got := next.Catalog.Presets[name]
			if got == nil || got.Prompt != p.Prompt {
				t.Fatalf("%s: preset %s prompt not kept", dir, name)
			}
			got.PromptFile = p.PromptFile
		}
		for name, sk := range cfg.Catalog.Skills {
			next.Catalog.Skills[name].Path = sk.Path
		}
		if !reflect.DeepEqual(next.Catalog, cfg.Catalog) {
			t.Errorf("%s: catalog changed in the round trip", dir)
		}
		for name, p := range cfg.Projects {
			q := next.Projects[name]
			q.APIVersion, q.Kind = p.APIVersion, p.Kind
			if !reflect.DeepEqual(q, p) {
				t.Errorf("%s: project %s changed in the round trip", dir, name)
			}
		}
	}
}

func TestPublishSwapsTheCurrentVersion(t *testing.T) {
	cfg, err := config.Load("../../examples/templates/config")
	if err != nil {
		t.Fatal(err)
	}
	cat, projects := cfg.Docs()
	edited := strings.Replace(string(cat), "stream: true", "stream: false", 1)
	next, err := cfg.WithDocs([]byte(edited), [][]byte{projects["template-project"]})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Current() != cfg {
		t.Fatal("before publishing, the loaded config is current")
	}
	cfg.Publish(next)
	if cfg.Current() != next || next.Current() != next || cfg.Current().Catalog.Planner.Stream {
		t.Fatal("publish did not swap the current version for every holder")
	}
	if (&config.Config{}).Current() == nil {
		t.Fatal("a literal config is its own current version")
	}
}

func TestWithDocsRejectsAnInvalidCatalog(t *testing.T) {
	cfg, err := config.Load("../../examples/templates/config")
	if err != nil {
		t.Fatal(err)
	}
	cat, projects := cfg.Docs()
	bad := strings.Replace(string(cat), "upstream: home", "upstream: nowhere", 1)
	if _, err := cfg.WithDocs([]byte(bad), [][]byte{projects["template-project"]}); err == nil || !strings.Contains(err.Error(), "unknown upstream") {
		t.Fatalf("got %v", err)
	}
	// A project whose repo grant is gone is rejected too.
	if _, err := cfg.WithDocs(cat, [][]byte{[]byte("metadata: {name: x}\nspec: {repo: repo/missing}\n")}); err == nil || !strings.Contains(err.Error(), "not a git grant") {
		t.Fatalf("got %v", err)
	}
}
