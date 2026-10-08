package resolve

import (
	"strings"
	"testing"

	"github.com/mauza/ai-flow/internal/config"
	"github.com/mauza/ai-flow/internal/flow"
)

func deployIssues(t *testing.T, nodes string) string {
	t.Helper()
	cfg := loadCfg(t)
	cfg.Env.Metrics.URL = "http://vm"
	cfg.Projects["sandbox"].Spec.Deploy = &config.Deploy{Branch: "main", Versions: []config.VersionProbe{{URL: "http://x/v"}}}
	f, err := flow.Parse([]byte("apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: { name: d, project: sandbox }\nspec:\n" + nodes))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range Validate(Resolve(f, cfg), cfg) {
		out = append(out, i.String())
	}
	return strings.Join(out, "\n")
}

const shipTail = `
    pr: { type: action, action: open_pull_request, next: { done: ci } }
    ci: { type: action, action: wait_for_checks, next: { passed: ship, failed: $fail, none: $fail, timeout: $fail } }
    ship: { type: action, action: merge_pull_request, next: { merged: live, conflict: $fail, blocked: $fail, timeout: $fail } }
    live: { type: action, action: wait_for_deploy, next: { deployed: soak, timeout: $fail } }
    soak: { type: action, action: check_health, next: { healthy: $success, degraded: revert, timeout: $fail } }
    revert:
      type: check
      grants: [repo/ai-flow-sandbox:write]
      inputs: { sha: "${{ nodes.ship.outputs.sha }}" }
      run: git revert --no-edit -m 1 "$AI_FLOW_INPUT_SHA"
      next: { pass: revert_pr, fail: $fail }
    revert_pr: { type: action, action: open_pull_request, next: { done: revert_ci } }
    revert_ci: { type: action, action: wait_for_checks, next: { passed: revert_ship, failed: $fail, none: $fail, timeout: $fail } }
    revert_ship: { type: action, action: merge_pull_request, next: { merged: $fail, conflict: $fail, blocked: $fail, timeout: $fail } }
`

const implement = `  start: work
  nodes:
    work:
      type: agent
      model: gpt-6.1-sol
      prompt: do it
      grants: [repo/ai-flow-sandbox:write]
      outcomes: [done]
      next: { done: pr }`

func TestSafeReleasePipelineWithRollback(t *testing.T) {
	if got := deployIssues(t, implement+shipTail); strings.Contains(got, "error") {
		t.Fatalf("a CI-gated, monitored pipeline with rollback should validate:\n%s", got)
	}
}

func TestDeploySafetyRules(t *testing.T) {
	for _, c := range []struct{ name, nodes, want string }{
		{"merge without CI", strings.Replace(implement+shipTail, "next: { done: ci } }\n    ci:", "next: { done: ship } }\n    ci:", 1),
			"node ship: merging deploys to production (main), but a path reaches this merge without a passing wait_for_checks"},
		{"commit after CI", strings.Replace(implement+shipTail, "next: { passed: ship, failed: $fail", "next: { passed: polish, failed: $fail", 1) + `    polish: { type: agent, model: gpt-6.1-sol, prompt: tidy, grants: [repo/ai-flow-sandbox:write], outcomes: [done], next: { done: ship } }
`, "node ship: merging deploys to production"},
		{"no monitoring", strings.Replace(implement+shipTail, "next: { merged: live,", "next: { merged: $success,", 1),
			"node ship.next.merged: after this deploy the run can reach $success without check_health reporting healthy"},
		{"health before deploy", strings.Replace(strings.Replace(implement+shipTail, "next: { merged: live,", "next: { merged: soak,", 1), "healthy: $success, degraded: revert", "healthy: live, degraded: revert", 1) +
			"", "node soak: checks health before wait_for_deploy has seen the new version"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := deployIssues(t, c.nodes); !strings.Contains(got, c.want) {
				t.Fatalf("missing %q in:\n%s", c.want, got)
			}
		})
	}
}

func TestNoDeployRulesWhenMergingDoesNotDeploy(t *testing.T) {
	cfg := loadCfg(t) // sandbox has no deploy config: a merge is just a merge
	f, _ := flow.Parse([]byte("apiVersion: ai-flow/v1alpha1\nkind: Flow\nmetadata: { name: d, project: sandbox }\nspec:\n" +
		strings.Replace(implement+shipTail, "next: { merged: live,", "next: { merged: $success,", 1)))
	for _, i := range Validate(Resolve(f, cfg), cfg) {
		if strings.Contains(i.Message, "deploy") {
			t.Errorf("unexpected: %s", i)
		}
	}
}
