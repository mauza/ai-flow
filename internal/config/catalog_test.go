package config_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"sigs.k8s.io/yaml"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
	"github.com/mauza/ai-flow/internal/tmpl"
)

func loadCatalog(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCatalogModelOptions(t *testing.T) {
	cfg := loadCatalog(t)
	want := []string{"gpt-6.1-sol", "gpt-6-luna", "gpt-6-astra"}
	if _, ok := cfg.Catalog.Models["gpt-6-sol"]; ok {
		t.Error("catalog must not offer the previous GPT-6 Sol alias")
	}
	if len(cfg.Catalog.Models) != len(want) {
		t.Fatalf("catalog must offer only Sol, Luna, and Astra: %v", cfg.Catalog.Models)
	}
	for _, name := range want {
		m := cfg.Catalog.Models[name]
		if m == nil {
			t.Fatalf("missing model %s", name)
		}
		if m.Upstream != "home" || m.Model != name {
			t.Errorf("%s must use the matching home alias: %+v", name, m)
		}
		if m.Size != "frontier" || !m.Reasoning || m.ToolUse != "good" || m.Cost != "subscription" || m.ContextTokens != 100000 {
			t.Errorf("%s has unexpected selection metadata: %+v", name, m)
		}
		if m.LLM != nil && len(m.LLM.Fallbacks) != 0 {
			t.Errorf("%s must not inherit model fallbacks: %v", name, m.LLM.Fallbacks)
		}
	}
	if cfg.Catalog.Planner.Model != want[0] || !cfg.Catalog.Planner.Stream {
		t.Fatalf("default planner must stream with Sol: %+v", cfg.Catalog.Planner)
	}
	if got := cfg.Projects["sandbox"].Spec.Allow.Models; !slices.Equal(got, want) {
		t.Errorf("sandbox must allow exactly the current catalog choices: %v", got)
	}
}

func TestCatalogPresetsResolve(t *testing.T) {
	cfg := loadCatalog(t)
	categories, types := map[string]bool{}, map[string]bool{}
	for name, preset := range cfg.Catalog.Presets {
		t.Run(name, func(t *testing.T) {
			if preset.Category == "" || preset.WhenToUse == "" || len(preset.Requires) == 0 || preset.Description == "" {
				t.Fatal("preset lacks selection metadata")
			}
			categories[preset.Category], types[preset.Type] = true, true
			legacy := slices.Contains([]string{"triage", "implement", "write-failing-test", "code-review"}, name)
			if legacy && (preset.MaxVisits != 0 || preset.OnExhausted != "") {
				t.Fatal("original preset must retain caller-configured visit limits")
			}
			if !legacy && (preset.MaxVisits < 1 || preset.OnExhausted != flow.Fail) {
				t.Fatal("preset must provide a bounded visit default")
			}
			node := &flow.Node{Uses: "preset/" + name, Next: map[string]string{}}
			if preset.Type == flow.TypeLLM || preset.Type == flow.TypeAgent {
				node.Model = cfg.Catalog.Planner.Model
			}
			if preset.Category == "implementation" {
				node.Grants = []string{"repo/ai-flow-sandbox:write"}
			}
			f := &flow.Flow{
				APIVersion: flow.APIVersion, Kind: flow.Kind,
				Metadata: flow.Metadata{Name: "catalog-" + name, Project: "sandbox"},
				Spec:     flow.Spec{Start: "step", Nodes: map[string]*flow.Node{"step": node}},
			}
			// Route the resolver's effective outcomes, including implicit ones.
			for i, outcome := range resolve.Resolve(f, cfg).Nodes["step"].Outcomes {
				node.Next[outcome] = flow.Fail
				if i == 0 {
					node.Next[outcome] = flow.Success
				}
			}
			if name == "custom-check" {
				issues := resolve.Validate(resolve.Resolve(f, cfg), cfg)
				if !slices.ContainsFunc(issues, func(i resolve.Issue) bool {
					return i.Severity == resolve.Error && i.Node == "step" && i.Field == "run"
				}) {
					t.Fatalf("unconfigured custom check was accepted: %v", issues)
				}
				node.Run = "python3 -m unittest discover -v"
			}
			data, err := yaml.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := flow.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			r := resolve.Resolve(parsed, cfg)
			if issues := resolve.Validate(r, cfg); len(issues) != 0 {
				t.Fatalf("one-node flow: %v", issues)
			}
			n := r.Nodes["step"]
			if n.Type != preset.Type || !reflect.DeepEqual(n.Outputs, preset.Outputs) || !reflect.DeepEqual(n.Outcomes, preset.Outcomes) {
				t.Fatalf("preset contract changed during resolution: %+v", n)
			}
			if n.Type == flow.TypeGate && (n.Timeout.Duration != 0 || n.Runtime != "") {
				t.Fatal("gate inherited pod execution defaults")
			}
			if n.Type == flow.TypeCheck && !reflect.DeepEqual(n.Outputs, map[string]any{"exit_code": "integer", "log_tail": "string"}) {
				t.Fatal("check outputs differ from the runner's actual contract")
			}
			if n.Type == flow.TypeAction && !slices.Contains(cfg.Catalog.Actions, n.Action) {
				t.Fatal("preset advertises an unsupported action")
			}
			// Compile actual result schemas and validate positive and negative
			// examples, rather than checking only that output names exist.
			schemaJSON, err := json.Marshal(resolve.ResultSchema(n))
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(schemaJSON, &schema); err != nil {
				t.Fatal(err)
			}
			compiled, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			outputs := map[string]any{}
			for field, typ := range n.Outputs {
				if _, err := resolve.OutputSchema(typ); err != nil {
					t.Fatalf("output %s: %v", field, err)
				}
				outputs[field] = exampleOutput(t, typ)
			}
			for _, outcome := range n.Outcomes {
				sample := map[string]any{"outcome": outcome, "summary": "Evidence from this step", "outputs": outputs}
				if err := compiled.Validate(sample); err != nil {
					t.Fatalf("valid result rejected: %v", err)
				}
			}
			for field, value := range outputs {
				outputs[field] = nil
				if err := compiled.Validate(map[string]any{"outcome": n.Outcomes[0], "summary": "", "outputs": outputs}); err == nil {
					t.Errorf("invalid output %s accepted", field)
				}
				outputs[field] = value
			}
			ctx := exampleContext(2, 20)
			texts := []string{n.Prompt}
			for _, value := range n.Inputs {
				texts = append(texts, value)
			}
			for _, value := range n.With {
				if text, ok := value.(string); ok {
					texts = append(texts, text)
				}
			}
			for _, text := range texts {
				if _, missing := tmpl.Render(text, ctx); len(missing) > 0 {
					t.Errorf("unusable template %q: %v", text, missing)
				}
			}
			for _, cs := range n.Cases {
				if err := resolve.CompileCEL(cs.When); err != nil {
					t.Fatal(err)
				}
				if _, err := resolve.EvalCEL(cs.When, ctx); err != nil {
					t.Fatalf("CEL fails against runtime-shaped context: %v", err)
				}
			}
		})
	}
	for _, category := range []string{"discovery", "planning", "implementation", "review", "verification", "control", "delivery"} {
		if !categories[category] {
			t.Errorf("no presets in %s", category)
		}
	}
	for _, typ := range flow.NodeTypes {
		if !types[typ] {
			t.Errorf("no presets for %s", typ)
		}
	}
	for name, field := range map[string]string{"triage": "questions", "implement": "summary_of_change", "write-failing-test": "test_name", "code-review": "comments"} {
		if p := cfg.Catalog.Presets[name]; p == nil || p.Outputs[field] == nil {
			t.Errorf("original preset contract lost: %s.%s", name, field)
		}
	}
}

func exampleOutput(t *testing.T, typ any) any {
	t.Helper()
	switch value := typ.(type) {
	case string:
		switch value {
		case "string":
			return "evidence"
		case "integer", "number":
			return 1.0
		case "bool", "boolean":
			return true
		}
	case []any:
		return []any{exampleOutput(t, value[0])}
	}
	t.Fatalf("add a meaningful example for output type %v", typ)
	return nil
}

func exampleContext(files, lines float64) map[string]any {
	return map[string]any{
		"task": map[string]any{"title": "Fix parsing", "body": "Handle empty input"},
		"run": map[string]any{"id": "run-1", "branch": "ai-flow/test", "pr_url": "https://example.test/pr/1",
			"diff": map[string]any{"files_changed": files, "lines_changed": lines}},
		"nodes": map[string]any{}, "inputs": map[string]any{},
	}
}

func TestCatalogSwitchBoundaries(t *testing.T) {
	cfg := loadCatalog(t)
	for _, tc := range []struct {
		name         string
		files, lines float64
		want         string
	}{
		{"has-changes", 0, 0, "unchanged"}, {"has-changes", 1, 0, "changed"},
		{"large-change", 10, 300, "small"}, {"large-change", 11, 0, "large"}, {"large-change", 1, 301, "large"},
	} {
		p := cfg.Catalog.Presets[tc.name]
		got := p.Default
		for _, cs := range p.Cases {
			match, err := resolve.EvalCEL(cs.When, exampleContext(tc.files, tc.lines))
			if err != nil {
				t.Fatal(err)
			}
			if match {
				got = cs.Outcome
				break
			}
		}
		if got != tc.want {
			t.Errorf("%s files=%v lines=%v: got %s, want %s", tc.name, tc.files, tc.lines, got, tc.want)
		}
	}
}

// Exercise the checked-in commands with passing, failing and missing-project
// fixtures. No cluster, model endpoints, package downloads or repo commits.
func TestCatalogCheckCommands(t *testing.T) {
	cfg := loadCatalog(t)
	for _, tc := range []struct {
		preset, tool string
		pass, fail   map[string]string
	}{
		{"go-test", "go", map[string]string{"go.mod": "module example.test/check\n\ngo 1.20\n", "check_test.go": "package check\nimport \"testing\"\nfunc TestCheck(t *testing.T) {}\n"},
			map[string]string{"check_test.go": "package check\nimport \"testing\"\nfunc TestCheck(t *testing.T) { t.Fatal(\"regression\") }\n"}},
		{"python-unittest", "python3", map[string]string{"test_check.py": "import unittest\nclass Check(unittest.TestCase):\n def test_check(self): self.assertEqual(1, 1)\n"},
			map[string]string{"test_check.py": "import unittest\nclass Check(unittest.TestCase):\n def test_check(self): self.fail('regression')\n"}},
		{"npm-test", "npm", map[string]string{"package.json": `{"scripts":{"test":"node -e 'process.exit(0)'"}}`},
			map[string]string{"package.json": `{"scripts":{"test":"node -e 'process.exit(1)'"}}`}},
		{"npm-build", "npm", map[string]string{"package.json": `{"scripts":{"build":"node -e 'process.exit(0)'"}}`},
			map[string]string{"package.json": `{"scripts":{"build":"node -e 'process.exit(1)'"}}`}},
		{"json-check", "python3", map[string]string{"data.json": `{"valid":true}`}, map[string]string{"data.json": `{"invalid":NaN}`}},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			for _, tool := range []string{"bash", "git", tc.tool} {
				if _, err := exec.LookPath(tool); err != nil {
					t.Skipf("%s unavailable: %v", tool, err)
				}
			}
			p := cfg.Catalog.Presets[tc.preset]
			if output, err := exec.Command("bash", "-n", "-c", p.Run).CombinedOutput(); err != nil {
				t.Fatalf("invalid bash: %v: %s", err, output)
			}
			dir := t.TempDir()
			if tc.preset == "json-check" {
				command(t, dir, "git", "init", "-q")
			}
			writeFixtures(t, dir, tc.pass)
			if tc.preset == "json-check" {
				command(t, dir, "git", "add", "--", "data.json")
			}
			run := func(dir, outcome string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "bash", "-o", "pipefail", "-c", p.Run)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "CI=true", "NO_COLOR=1",
					"GOPROXY=off", "GOTOOLCHAIN=local", "GOCACHE=" + filepath.Join(t.TempDir(), "go-cache"), "PYTHONDONTWRITEBYTECODE=1"}
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("check timed out: %s", output)
				}
				got := "pass"
				if err != nil {
					got = "fail"
				}
				if got != outcome {
					t.Fatalf("got %s, want %s: %v\n%s", got, outcome, err, output)
				}
			}
			run(dir, "pass")
			writeFixtures(t, dir, tc.fail)
			run(dir, "fail")
			empty := t.TempDir()
			if strings.HasPrefix(tc.preset, "npm-") {
				// npm test can fall back to node test.js without scripts.test.
				writeFixtures(t, empty, map[string]string{"package.json": `{}`, "test.js": "process.exit(0)"})
			}
			if tc.preset == "json-check" {
				command(t, empty, "git", "init", "-q")
			}
			run(empty, "fail")
		})
	}
}

func writeFixtures(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func command(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", name, err, out)
	}
}
