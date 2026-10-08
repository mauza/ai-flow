package flow

import (
	"sort"
	"time"
)

// ActionSpec describes a built-in action: what it can emit, what it accepts,
// and whether it waits. A waiting action is polled by the engine across ticks
// until it finishes or its timeout passes, which adds a `timeout` outcome.
type ActionSpec struct {
	Outcomes []string
	With     []string // accepted `with` keys
	Outputs  map[string]any
	Waits    bool
	Timeout  time.Duration // default when the node sets none (waiting actions)
	Doc      string        // one line for the planner menu
}

// Actions is every built-in action.
var Actions = map[string]ActionSpec{
	"open_pull_request": {
		Outcomes: []string{"done"}, With: []string{"title", "body", "draft"},
		Outputs: map[string]any{"url": "string", "number": "integer"},
		Doc:     "opens (or updates) a PR from the run branch. Outcome: done.",
	},
	"comment_task": {
		Outcomes: []string{"done"}, With: []string{"body"},
		Doc: "comments on the source task. Outcome: done.",
	},
	"merge_pull_request": {
		Outcomes: []string{"merged", "conflict", "blocked"}, With: []string{"method"},
		Outputs: map[string]any{"sha": "string", "previous_sha": "string", "number": "integer", "url": "string"},
		Waits:   true, Timeout: 10 * time.Minute,
		Doc: "merges the run's PR (method: merge by default, so it can be reverted). In a project whose deploy branch is the PR base, merging IS the production deploy. Outcomes: merged (outputs.sha), conflict, blocked, timeout.",
	},
	"wait_for_checks": {
		Outcomes: []string{"passed", "failed", "none"}, With: []string{"ref", "checks", "settle"},
		Outputs: map[string]any{"ref": "string", "failed": []any{"string"}, "checks": []any{"string"}},
		Waits:   true, Timeout: 45 * time.Minute,
		Doc: "waits for GitHub CI (check runs and statuses) on ref (default: the run branch head; use a merge sha for post-merge build/deploy workflows); checks: names to require. Outcomes: passed, failed (outputs.failed), none (no CI reported within settle, default 2m), timeout.",
	},
	"wait_for_deploy": {
		Outcomes: []string{"deployed"}, With: []string{"expect"},
		Outputs: map[string]any{"version": "string"},
		Waits:   true, Timeout: 20 * time.Minute,
		Doc: "waits until every version endpoint of the project's deploy target reports the expected commit (default: the run's latest merge). Outcomes: deployed, timeout.",
	},
	"rollback_deploy": {
		Outcomes: []string{"rolled_back", "failed"}, With: []string{"sha"},
		Outputs: map[string]any{"url": "string", "previous": "string"},
		Waits:   true, Timeout: 10 * time.Minute,
		Doc: "fast rollback without a rebuild: dispatches the project's deploy.rollback workflow for the release (default: the run's latest merge) and waits for it. Follow with wait_for_deploy expecting ${{ run.previous_sha }}. Outcomes: rolled_back, failed, timeout.",
	},
	"check_health": {
		Outcomes: []string{"healthy", "degraded"}, With: []string{"duration"},
		Outputs: map[string]any{"violations": []any{"string"}, "polls": "integer"},
		Waits:   true, Timeout: 30 * time.Minute,
		Doc: "soaks the deployed app for duration (default 10m): the project's health PromQL queries and URL probe, polled; a check failing twice in a row is degraded. Outcomes: healthy, degraded (outputs.violations), timeout.",
	},
}

// ActionNames lists the built-in actions in a stable order.
func ActionNames() []string {
	names := make([]string, 0, len(Actions))
	for n := range Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
