package config_test

import (
	"os"
	"path/filepath"
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

func TestUpstreamBaseURLFromEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(env string) {
		os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(env+`
---
apiVersion: ai-flow/v1alpha1
kind: Catalog
models:
  m: { upstream: home, model: m }
runtimes: {}
`), 0o644)
	}
	write("kind: Environment\nllm:\n  upstreams:\n    home: { baseUrl: \"https://default.example/v1\", baseUrlEnv: TEST_LLM_BASE_URL }")
	t.Setenv("TEST_LLM_BASE_URL", "http://mine.example/v1")
	cfg, err := config.Load(dir)
	if err != nil || cfg.Env.LLM.Upstreams["home"].BaseURL != "http://mine.example/v1" {
		t.Fatalf("env override: %+v %v", cfg, err)
	}
	t.Setenv("TEST_LLM_BASE_URL", "")
	if cfg, _ = config.Load(dir); cfg.Env.LLM.Upstreams["home"].BaseURL != "https://default.example/v1" {
		t.Fatal("an unset variable keeps baseUrl")
	}
	write("kind: Environment\nllm:\n  upstreams:\n    home: { baseUrlEnv: TEST_LLM_BASE_URL }")
	if _, err := config.Load(dir); err == nil || !strings.Contains(err.Error(), "baseUrl is required") {
		t.Fatalf("got %v", err)
	}
}
