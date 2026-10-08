# Node catalog

`deploy/config/catalog.yaml` provides 38 reusable primitives. They use six of the
node types: `llm`, `agent`, `check`, `gate`, `switch`, and `action`. `parallel` and
`join` are structure rather than presets: wrap independent read-only presets (checks,
reviews, investigations) in a parallel node to run them at once; see
[SPEC §4.1.1](SPEC.md#411-parallel-branches).
Pick the few steps needed for a task; a catalog is a menu, not a mandatory pipeline.

## Selection guide

| Category | Presets | Use for |
| --- | --- | --- |
| Discovery | `triage`, `repo-map`, `investigate-bug`, `dependency-audit` | Clarify the task, locate code/commands, reproduce failures, inventory dependencies. |
| Planning | `acceptance-criteria`, `implementation-plan`, `test-plan`, `compare-approaches` | Turn supplied evidence into bounded decisions and testable work. |
| Implementation | `implement`, `write-failing-test`, `fix-tests`, `address-review`, `refactor`, `update-docs` | Focused repository edits with evidence. Requires the project repo's write grant. |
| Review | `code-review`, `security-review`, `test-review` | Judge the supplied diff and context; route actionable findings back to edits. |
| Verification | `go-test`, `python-unittest`, `npm-test`, `npm-build`, `json-check`, `custom-check` | Execute deterministic commands in a compatible runtime. |
| Control | `human-decision`, `has-changes`, `large-change` | Optional human decisions or deterministic routing on the cumulative branch diff. |
| Delivery | `summarize-change`, `release-notes`, `open-pull-request`, `comment-task` | Prepare evidence-based text, then publish through existing actions when appropriate. |
| Release | `ci-checks`, `local-stack-test`, `qa-test`, `deploy`, `wait-for-deploy` | Verify in CI and locally (QA exercises the app), then ship by merging. |
| Operations | `monitor-deploy`, `deploy-triage`, `revert-deploy` | Soak production on health queries; on degradation diagnose and revert. |

## Release: ship on merge, watch, roll back

A project ships to production by merging into its `deploy.branch` (its CI/CD
builds and rolls out that branch). Declare it in the Project:

```yaml
deploy:
  branch: main
  url: https://games.example            # probed during the soak (2xx)
  versions:                             # all must report the merged commit
    - { url: https://games.example/api/version, field: commit }
  health:                               # PromQL against Environment metrics.url
    - { name: 5xx rate, query: 'sum(rate(traefik_service_requests_total{service=~"games.*",code=~"5.."}[5m])) or vector(0)', max: 0.05 }
    - { name: restarts, query: 'sum(increase(kube_pod_container_status_restarts_total{namespace="games"}[10m]))', max: 0 }
```

The release pipeline, in catalog terms:

```
work → (unit tests ‖ lint ‖ local-stack-test ‖ qa-test) → open-pull-request → ci-checks
  → deploy → wait-for-deploy → monitor-deploy → healthy: $success
                                              → degraded: deploy-triage → revert-deploy
                                                → open-pull-request → ci-checks → deploy → $fail
```

- `ci-checks` (`wait_for_checks`) waits on the PR head's GitHub check runs and
  statuses; `none` means no CI reported within `settle` (default 2m).
- `deploy` (`merge_pull_request`) merges at the PR head with a merge commit, so
  `revert-deploy` can revert it and push the run branch as a fast-forward.
- `wait-for-deploy` polls `deploy.versions` until every endpoint reports the merged
  commit; `monitor-deploy` (`check_health`) then soaks: a check failing on two
  consecutive polls is degraded; query errors are inconclusive (a metrics outage
  ends in `timeout`, not a rollback). Write queries where no data means healthy.
- `revert-deploy` reads `run.merged_sha` (the run's latest merge) and `run.base`
  through `$AI_FLOW_INPUT_*`; give it the repo write grant.

When merging deploys, the validator rejects a flow in which any merge can be
reached without a passing `ci-checks` after the last repo-writing step, or in which
the run can succeed after a merge without `wait-for-deploy` and then a healthy
`monitor-deploy`. Projects without `deploy` (or whose flows target another base)
merge without these rules.

The original four names and output fields remain available: `triage.questions`,
`implement.summary_of_change`, `write-failing-test.test_name/evidence`, and
`code-review.comments`. New presets declare their exact outputs and outcomes in
the catalog and overview API; the planner sees typed outputs and selection cues.

## Metadata and API

A `config.Preset` embeds `flow.Node` and adds optional discovery metadata:

```yaml
category: verification             # string
when_to_use: A Go module needs tests. # string
requires:                          # []string: capability/precondition hints
  - agent-go with a compatible Go version
min_size: medium                   # model-size hint for llm/agent, when applicable
```

`requires` and `min_size` are guidance, not capability enforcement or automatic
setup. They do not grant access, install tools, or add fields to `flow.Node`.
Existing catalogs without the new metadata still load.

`GET /api/overview` returns each preset with these fields:

```text
name, type, description, category, when_to_use, min_size: string
requires: string[]                  # [] when absent
outcomes: string[]
outputs: map[string]outputType | null
definition: flow.Node               # full raw preset node, not resolved defaults
```

Output types use the existing shorthand (`string`, `integer`, `number`, `bool`,
`[string]`) or supported JSON Schema maps. Model results contain `outcome`,
`summary`, and `outputs`; all declared output fields belong in `outputs`, including
empty arrays/strings when appropriate. Checks actually emit `exit_code: integer`
and `log_tail: string`. A check may also declare
`outputs` and write them as one JSON object to the file named by `$AI_FLOW_OUTPUTS`,
e.g. `go test -json ./... | ./summarize > "$AI_FLOW_OUTPUTS"` producing
`{"failed": 2, "failing": ["TestSlug"]}`. Fields are type-checked against the
declaration; an undeclared field, a wrong type or a file that is not a JSON object
fails the step. No file means no structured outputs, so a command that fails early
still routes by its exit code. PR creation emits `url: string` and `number: integer`.
Gates, switches, and task comments declare no extra output payload.

The planner menu includes category, when-to-use, requirements, output types,
outcomes, visit bounds, and executable check/switch/action settings. Full model
prompts stay in the catalog to keep planning context compact.

## Compose and configure

Reference `uses: preset/<name>`. Set a model on `llm`/`agent` nodes, grant
`<project-repo>:write` for edits, and route **every** outcome in `next`.
Node settings override preset settings. Maps/lists such as `with`, `inputs`,
`outputs`, and `cases` are replaced as a whole, not merged.

```yaml
spec:
  start: implement
  nodes:
    implement:
      uses: preset/implement
      model: gpt-6.1-sol
      grants: [repo/ai-flow-sandbox:write]
      max_visits: 3
      on_exhausted: $fail
      next: {done: test, stuck: $fail}
    test:
      uses: preset/python-unittest
      next: {pass: review, fail: implement}
    review:
      uses: preset/code-review
      model: gpt-6-luna
      next: {approve: $success, changes: implement}
```

This fragment assumes the sandbox project. For other repos, select the appropriate
check/runtime and change the grant. Add delivery only if the task calls for it.
For non-adjacent evidence, bind explicit inputs, e.g.
`inputs: {review: '${{ nodes.review.outputs.comments }}'}`. A generated PR body can
be wired with a complete map:
`with: {title: '${{ nodes.summary.outputs.title }}', body: '${{ nodes.summary.outputs.body }}', draft: false}`.

## Runtime constraints

- **Models:** `llm` has no tools; repository discovery belongs in an `agent`.
  Choose the smallest sufficient configured model for each step. The only model
  options are `gpt-6.1-sol` (default planner, `planner.stream: true`), `gpt-6-luna`,
  and `gpt-6-astra`, all using `home` with matching upstream aliases. No model fallback
  is configured. All three declare `size: frontier`, `reasoning: true`, `tool_use: good`,
  and `cost: subscription` as selection metadata, not performance claims.
  Their `context_tokens: 100000` is a conservative working limit, not full capacity.
  Earlier streaming smoke tests covered GPT-6 Sol (the previous version) and Luna with the existing
  OpenCode login; see [CHATGPT-PROVIDER.md](CHATGPT-PROVIDER.md) for setup and limits.
- **Images:** `agent-base` includes bash, git, ripgrep, Python 3, Node 22/npm,
  jq, and curl. `agent-go` adds Go 1.26 in the checked-in Dockerfile. Projects
  needing a newer Go version (including ai-flow itself) need a compatible runtime
  or explicitly configured toolchain setup. Neither image implies project dependencies.
- **Isolation:** each pod gets a fresh checkout. Uncommitted files and dependency
  installs do not persist to the next node. Only granted repository changes are
  committed. Install dependencies within the same check's `run`, or provide them
  in its runtime; package downloads/services need permitted connectivity.
- **Shell:** checks run `bash -o pipefail -c` from the repo with `CI=true` and
  `NO_COLOR=1`, using a filtered environment. There is no implicit `set -e`.
  Use `&&` or `set -e` for setup plus verification; do not interpolate untrusted
  task/model text into shell commands. `run` is a literal command, not a template.
- **Checks:** `go-test` runs `go test -count=1 ./...`; inspect whether relevant tests
  actually exist. `python-unittest` uses standard-library discovery and rejects
  zero tests. `npm-test` requires an explicit root `scripts.test` (no implicit
  `test.js` fallback); `npm-build` requires `scripts.build`. Configure non-watch
  scripts and dependencies. `json-check` parses tracked strict JSON, rejects NaN/
  Infinity and an empty file selection, and does not support JSONC.
- **Custom checks:** `custom-check` intentionally has no `run`. Existing flow
  validation rejects it until configured with a real command, such as
  `run: npm ci --no-audit --no-fund && npm run typecheck`, using the right runtime
  and permitted package access. No passing placeholder is supplied.
- **Control:** `has-changes` tests `files_changed > 0`; `large-change` tests
  `files_changed > 10 || lines_changed > 300`. Tune `cases` to the project.
  Neither mandates a human gate. `human-decision` waits for approve/changes/stop
  without an inherited pod timeout; tailor its question to task/project guidance.
- **Bounds:** new edits/reviews default to 3 visits, discovery/planning/model delivery
  to 2, checks to 4, switches to 6, gates to 2, publishing actions to 1;
  exhaustion routes to `$fail`. The original four presets retain caller-configured
  limits; set `max_visits: 3` on `implement` when looping. Raise bounds deliberately for larger workflows.
  Visits and infrastructure retry attempts are different limits.
- **Publishing:** PRs require a GitHub-backed repo, configured control-plane
  credentials, and a pushed run branch. `comment-task` requires a linked task and
  a comment-capable hook; the existing runtime may report done without publishing
  when those are absent. Its default body is a run reference; override it for a
  generated handoff. Action failures are runtime errors, not invented outcomes.

## Verification

`go test ./internal/config ./internal/planner ./internal/server` checks one-node
flows for every preset, real output schemas and CEL contexts, template references,
shell commands on passing/failing/missing-project fixtures, planner contracts and
context size, and overview definitions. Command tests skip if a required local
tool is unavailable. These checks do not contact models, GitHub, or a cluster.
