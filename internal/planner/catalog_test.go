package planner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
)

func TestPresetMenuContracts(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	writePresetMenu(&sb, cfg.Catalog.Presets)
	menu := sb.String()
	// A generous byte budget guards against accidentally dumping full prompts
	// or result schemas for every preset (roughly 6k tokens at this ceiling).
	if len(menu) > 24000 {
		t.Fatalf("preset menu grew to %d bytes", len(menu))
	}
	t.Logf("preset menu: %d bytes for %d presets", len(menu), len(cfg.Catalog.Presets))
	for name, p := range cfg.Catalog.Presets {
		t.Run(name, func(t *testing.T) {
			prefix := "- `preset/" + name + "`"
			var line string
			for _, candidate := range strings.Split(menu, "\n") {
				if strings.HasPrefix(candidate, prefix) {
					if line != "" {
						t.Fatal("duplicate menu entry")
					}
					line = candidate
				}
			}
			if line == "" {
				t.Fatal("missing menu entry")
			}
			for _, text := range append([]string{p.Description, p.WhenToUse, strings.Join(p.Outcomes, ", ")}, p.Requires...) {
				if !strings.Contains(line, text) {
					t.Errorf("missing selection cue %q", text)
				}
			}
			if !strings.Contains(menu, "### "+p.Category+"\n") {
				t.Fatal("missing category")
			}
			if len(p.Outputs) > 0 && !strings.Contains(line, "Outputs: "+inline(p.Outputs)) {
				t.Fatal("missing typed outputs")
			}
			if p.Type == flow.TypeLLM || p.Type == flow.TypeAgent {
				if strings.Contains(menu, strings.TrimSpace(p.Prompt)) {
					t.Fatal("full model prompt leaked into selection menu")
				}
				if !strings.Contains(line, "min model "+p.MinSize) {
					t.Fatal("missing model size hint")
				}
			}
			_, encoded, found := strings.Cut(line, " Defaults: ")
			if !found {
				if p.MaxVisits > 0 || p.Runtime != "" || len(p.Inputs) > 0 || (p.Type != flow.TypeLLM && p.Type != flow.TypeAgent) {
					t.Fatal("missing wiring defaults")
				}
				return
			}
			var defaults flow.Node
			if err := json.Unmarshal([]byte(encoded), &defaults); err != nil {
				t.Fatalf("wiring is not valid node JSON: %v", err)
			}
			if defaults.Runtime != p.Runtime || defaults.MaxVisits != p.MaxVisits || defaults.OnExhausted != p.OnExhausted {
				t.Fatal("execution defaults lost")
			}
			switch p.Type {
			case flow.TypeCheck:
				if defaults.Run != p.Run || !reflect.DeepEqual(defaults.ExitCodes, resolve.CheckExitCodes(&p.Node)) {
					t.Fatal("check wiring lost")
				}
				if p.Run == "" && !strings.Contains(line, "Configure run: REQUIRED") {
					t.Fatal("unconfigured check not identified")
				}
			case flow.TypeSwitch:
				if !reflect.DeepEqual(defaults.Cases, p.Cases) || defaults.Default != p.Default {
					t.Fatal("switch conditions lost")
				}
			case flow.TypeAction:
				if defaults.Action != p.Action || !reflect.DeepEqual(defaults.With, p.With) {
					t.Fatal("action wiring lost")
				}
			case flow.TypeGate:
				if defaults.Prompt != p.Prompt {
					t.Fatal("human decision lost")
				}
			}
		})
	}
	var again strings.Builder
	writePresetMenu(&again, cfg.Catalog.Presets)
	if again.String() != menu {
		t.Fatal("menu ordering is nondeterministic")
	}
}

func TestCatalogPlanningGuidanceAndExample(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	prompt := New(cfg, nil, nil).systemPrompt(cfg.Projects["sandbox"])
	for _, cue := range []string{"configured planner for decomposition", "smallest sufficient configured model", "Gates are optional", "bash -o pipefail -c", "fresh checkout", "replaced, not merged", "# Choosing rigor", "Importance alone earns neither"} {
		if !strings.Contains(prompt, cue) {
			t.Errorf("missing planning constraint %q", cue)
		}
	}
	for _, old := range []string{"Use local models for everything", "small local models", "cheap local models", "When you leave gates out of risky work"} {
		if strings.Contains(prompt, old) {
			t.Errorf("old model/gate policy remains: %q", old)
		}
	}
	f, err := flow.Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	f.Metadata.Project = "sandbox"
	f.Spec.Nodes["implement"].Grants = []string{"repo/ai-flow-sandbox:write"}
	if issues := resolve.Validate(resolve.Resolve(f, cfg), cfg); len(issues) > 0 {
		t.Fatalf("planner example no longer resolves: %v", issues)
	}
}

func TestPresetMenuLegacyMetadataOptional(t *testing.T) {
	var sb strings.Builder
	writePresetMenu(&sb, map[string]*config.Preset{"legacy": {
		Node: flow.Node{Type: flow.TypeLLM, Description: "Old preset", Outputs: map[string]any{"answer": "bool"}, Outcomes: []string{"done"}},
	}})
	for _, want := range []string{"preset/legacy", "Old preset", "answer: bool", "Outcomes: done"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("legacy preset missing %q", want)
		}
	}
}

func TestDeployingProjectGetsTheReleasePipeline(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, nil, nil)
	plain := p.systemPrompt(cfg.Projects["sandbox"])
	for _, a := range flow.ActionNames() {
		if !strings.Contains(plain, "- `"+a+"`: ") {
			t.Errorf("action %s missing from the menu", a)
		}
	}
	if strings.Contains(plain, "Deploys on merge") {
		t.Error("a project without deploy config must not get release instructions")
	}
	cfg.Projects["sandbox"].Spec.Deploy = &config.Deploy{Branch: "main", URL: "https://games.example"}
	prompt := p.systemPrompt(cfg.Projects["sandbox"])
	for _, cue := range []string{"## Deploys on merge", "Merging a PR into `main` ships to production", "(https://games.example)", "deploy (merge) → wait-for-deploy → monitor-deploy", "revert-deploy"} {
		if !strings.Contains(prompt, cue) {
			t.Errorf("missing %q", cue)
		}
	}
	if strings.Contains(prompt, "fast rollback (deploy.rollback)") {
		t.Error("fast rollback offered without deploy.rollback")
	}
	cfg.Projects["sandbox"].Spec.Deploy.Rollback = &config.RollbackSpec{Workflow: "rollback.yml"}
	prompt = p.systemPrompt(cfg.Projects["sandbox"])
	if !strings.Contains(prompt, "rollback-release → wait-for-rollback → deploy-triage") {
		t.Error("fast rollback path missing")
	}
}
