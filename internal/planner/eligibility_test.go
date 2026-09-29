package planner

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
	"github.com/mauza/ai-flow/internal/resolve"
)

func TestProjectEligibility(t *testing.T) {
	cfg, err := config.Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Models = map[string]*config.Model{"local-a": {}, "remote": {}, "local-b": {}}
	cfg.Catalog.Grants = map[string]*config.Grant{
		"repo/code":     {Kind: config.GrantGit},
		"repo/readonly": {Kind: config.GrantGit, Modes: []string{"read"}},
		"mcp/tools":     {Kind: config.GrantMCP},
	}
	for _, tc := range []struct {
		name           string
		allow          config.Allow
		models, grants []string
	}{
		{"empty", config.Allow{}, []string{"local-a", "local-b", "remote"}, []string{}},
		{"patterns", config.Allow{Models: []string{"local-*"}, Grants: []string{"repo/*:read", "mcp/*"}}, []string{"local-a", "local-b"}, []string{"mcp/tools", "repo/code", "repo/code:read", "repo/readonly", "repo/readonly:read"}},
		{"catalog modes", config.Allow{Models: []string{"missing"}, Grants: []string{"*/*:*"}}, []string{}, []string{"mcp/tools", "repo/code", "repo/code:read", "repo/code:write", "repo/readonly", "repo/readonly:read"}},
		// AllowsGrant deliberately accepts a bare name for any allowed mode;
		// the resolver then interprets that bare git name as read.
		{"write only policy", config.Allow{Grants: []string{"repo/code:write"}}, []string{"local-a", "local-b", "remote"}, []string{"repo/code", "repo/code:write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := &config.Project{Spec: config.ProjectSpec{Allow: tc.allow, Repo: "repo/code"}}
			cfg.Projects["sandbox"] = project
			got := ProjectEligibility(cfg, project)
			if !reflect.DeepEqual(got.AllowedModels, tc.models) || !reflect.DeepEqual(got.AllowedGrants, tc.grants) {
				t.Fatalf("got %+v, want models=%v grants=%v", got, tc.models, tc.grants)
			}
			for _, grant := range []string{"repo/code", "repo/code:read", "repo/code:write", "repo/readonly", "repo/readonly:read", "repo/readonly:write", "mcp/tools"} {
				f, err := flow.Parse([]byte("apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: {name: test, project: sandbox}\nspec:\n  start: check\n  nodes:\n    check:\n      type: check\n      run: true\n      grants: [\"" + grant + "\"]\n      next: {pass: $success, fail: $fail}\n"))
				if err != nil {
					t.Fatal(err)
				}
				allowed := true
				for _, issue := range resolve.Validate(resolve.Resolve(f, cfg), cfg) {
					if issue.Severity == resolve.Error && issue.Node == "check" && issue.Field == "grants" {
						allowed = false
					}
				}
				if allowed != slices.Contains(got.AllowedGrants, grant) {
					t.Errorf("resolver/menu disagree for %q", grant)
				}
			}
			prompt := New(cfg, nil, nil).systemPrompt(project)
			if !slices.Contains(got.AllowedGrants, "repo/code:write") && strings.Contains(prompt, "- `repo/code:write`") {
				t.Fatal("planner advertised disallowed write access")
			}
		})
	}
}
