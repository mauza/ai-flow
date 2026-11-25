# Repeat work until a goal is met

Yes: use ordinary nodes and a **back edge**. No loop engine, group node, or shared
mutable variable is needed. A switch repeats work automatically; an optional gate
lets a human choose whether another pass is useful.

## Two complete patterns

| Example | Repeated group | Completion / exhaustion |
| --- | --- | --- |
| [repeat-until-goal.yaml](../examples/loops/repeat-until-goal.yaml) | `work → verify → assess → condition → work` | Success requires a passing check **and** `assess.outputs.goal_met == true`. At most three work visits, then a one-time handoff gate ending in failure. |
| [human-revise-approve.yaml](../examples/loops/human-revise-approve.yaml) | `work → verify → review_gate`; `revise → work` | Passing checks reach human approval. Revise repeats work; stop, timeout, or exhausted work visits fail. At most three work visits. |

Both use actual catalog presets (`implement`, `python-unittest`, `code-review`,
`human-decision`) and the checked-in `sandbox` project. Work uses `gpt-6.1-sol`;
the automated assessor uses a different model, `gpt-6-luna`, so it does not
grade its own work. Before real execution,
provide concrete task criteria and specialize the project, repo write grant,
models, verification command, and runtime for the target repository. Python's
standard-library tests must be discoverable from the repo root; the preset fails
if it discovers none. Dependencies must be available in each fresh check pod.
Ending at `$success` means the stated acceptance path passed, not that a PR was
opened or merged.

Validate offline from the repo root:

```sh
go run ./cmd/ai-flow validate -config deploy/config \
  examples/loops/repeat-until-goal.yaml \
  examples/loops/human-revise-approve.yaml

go test ./internal/engine -run 'TestLoop' -v
```

Validation does not contact configured model endpoints or repositories. Engine
tests use synthetic node results and the real persisted state machine, not live
agents. For execution, save the specialized flow through the existing UI/API and
start it with your configured project and task.

## A condition is a result, not a mutable flag

The assessor declares:

```yaml
outputs: { goal_met: bool, remaining: [string] }
```

Each visit returns a structured result such as:

```json
{"outcome":"assessed","summary":"One criterion remains","outputs":{"goal_met":false,"remaining":["Handle empty input"]}}
```

The switch reads it with CEL:

```yaml
cases:
  - when: 'nodes.verify.outcome == "pass" && nodes.assess.outputs.goal_met == true'
    outcome: complete
default: repeat
next: { complete: $success, repeat: work }
```

`nodes.assess.outputs` is the **latest successful visit's persisted output** for
that node; earlier visits remain in history. A later result supersedes the values
seen through that name. `nodes.assess.history` lists **every** successful visit
(`visit`, `outcome`, `summary`, `outputs`), oldest first: read it in a template to
pass all earlier feedback on, or count passes in CEL with
`size(nodes.assess.history)`. Pod steps in a loop also get a *Loop history* context
section listing every pass of each repeated step with its outputs, so a fixer sees
the first review's comments as well as the latest. There is still no assignment,
increment or reset. Skipped producers can leave stale values visible;
these examples rerun verify/assess before every condition evaluation. Accessing
a missing value in CEL can fail the run; do not assume missing means false.
The initial work step uses template fallbacks because no earlier feedback exists.

Use named inputs to carry evidence past a switch or gate: those nodes become the
immediately previous step and do not reproduce the assessor's outputs. The examples
bind the latest findings/check log explicitly on the work node.

**Prefer deterministic evidence.** For "all tests pass," use the check's
`pass`/`fail` transition directly—no LLM assessor needed. Add a structured judgment
only for criteria that need interpretation. The first example combines both:
even `goal_met: true` cannot override a failed check. Model judgment alone is not
proof of implementation correctness. Changing a preset's `outputs` or `outcomes`
replaces that whole contract; the example also changes the assessor prompt to match.

## Bound visits and waiting separately

- `max_visits: 3` permits **three total entries of that node in this run**. It is
  not an iteration-counter variable and does not reset after success, a back edge,
  or a human decision. Returning for a fourth entry follows `on_exhausted` before
  launching work. Infrastructure retry attempts are a separate setting.
- Check every cycle and every preset limit in the repeated group. The examples
  explicitly align work/check/review limits to three so inherited limits do not
  unexpectedly cut a group short.
- A gate alone does **not** guarantee automatic termination. Although graph
  validation allows human-mediated cycles, use a visit bound to limit repetition
  and a timeout to limit each wait. A per-wait timeout is not a total-loop deadline.
- Both examples explicitly set gate `timeout: 24h` and route `timeout: $fail`.
  The catalog's `defaults.timeout: 20m` applies to pod nodes, **not gates**; project
  and flow pod defaults also do not supply gate timeouts. Adding a gate timeout
  creates an effective `timeout` outcome, which must be routed.
- Plan for cumulative waiting: the first example permits one 24h handoff; the
  second permits up to three separate 24h approval waits, plus execution time.
  Set `spec.budget.wall` and `spec.budget.tokens` (or `project.spec.budget.tokens_per_run`)
  to enforce run-wide wall-clock and token ceilings across all visits.

The exhaustion gate is an optional acknowledgment, not a bypass to success or a
way to reset the exhausted node. For fully automatic handling, route exhaustion
directly to `$fail` and remove the unused gate. Likewise, omit human approval
unless task/project guidance calls for it.

## What a human revision means

A gate decision records the selected outcome, who chose it, and an optional
`note` (`outputs.note` on the gate visit), which reaches the next step in its
context and templates. By default, `revise` still counts against `max_visits`;
passing `extend_visits: true` (or checking **Grant +1 loop round** in the UI)
raises the run's visit bound by 1 round (up to +10) so human-directed revisions
do not hit an artificial exhaustion wall.

While a run is active, you can also steer it mid-flight (`POST /api/runs/{id}/steer`,
the run view's **Steer** / **Pause after step** controls, or MCP `steer_run`) to
inject a direction note for subsequent steps or pause cleanly between steps at a
synthetic `$pause` gate without canceling the active step.

## Review budget (project `review`)

A project can say how hard its flows work to find and fix problems:

```yaml
spec:
  review:
    rounds: 10        # every bounded step (fix and review loops) gets at least this many visits
    mode: thorough    # repeat reviews re-check everything and block on any real defect
```

`rounds` raises `max_visits` for bounded nodes participating in cycles when a run
is resolved, without inflating acyclic nodes like `open-pull-request` or `deploy`.
`mode: converge` (the default) tells a review that runs again to block
only on findings still open and acceptance or test failures, so loops end;
`thorough` makes every pass a full review that may find new problems.

## Recurring work (project `schedules`)

A schedule queues work every so often. It has exactly one target: a saved `flow`,
a `task` to plan, or the next ready card of a `storymap`.

```yaml
spec:
  schedules:
    - name: nightly-audit
      every: 24h               # at least 5m
      auto_start: true         # start the planned run without a human
      max_active_runs: 1       # default 1
      max_open_tasks: 3        # default 5
      max_open_prs: 2          # default 2
      task:
        title: Improvement audit
        body: Use template/continuous-audit.
    - name: next-card
      every: 4h
      auto_start: true
      storymap: { map: core, max_attempts: 2 }
```

The limits apply to the whole project:

- `max_active_runs` counts active runs and the tasks that will start their own
  run, including those still being planned.
- `max_open_tasks` counts tasks being planned, queued or running. A task that
  waits for a human to start it does not count.
- `max_open_prs` counts open pull requests from `ai-flow/` branches. If GitHub
  can't be read, nothing starts.

A trigger that a limit skips, or that fails, is tried again 10 minutes later. If
several schedules are due, the one that ran longest ago goes first. Tasks that an
auto-started run creates with `create_tasks` start under the same schedule, oldest
first, as slots free up. Planning that a restart interrupted is retried once.

A story map schedule picks `todo` cards by release, then journey order, and skips
cards with open questions. A failed card is picked again after another interval,
with the failure in its brief, until `max_attempts`. A canceled card is not.

Pause a schedule on the project's Health & value tab, with
`POST /api/projects/{project}/schedules/{name}/pause`, or with the MCP tool
`pause_schedule`. A paused schedule also holds the tasks it queued. A forced
trigger (`force: true`) ignores the limits and the pause for that one task.

## Improvement ledger

Each project has a ledger of proposed and delivered improvements. Model steps see
it in their context, and `create_tasks` skips what it already covers.

| Status | Meaning |
| --- | --- |
| `proposed` | A task was created for it. |
| `pr_open` | Its run succeeded and the pull request is still open. |
| `shipped` | The pull request was merged, or the run needed none. |
| `rejected` | The pull request was closed without merging, or a human said no. |
| `reverted`, `wont_fix` | Set through the API or MCP. |

The scheduler checks `pr_open` entries every 10 minutes. `create_tasks`
identifies an item by its `key` within the project. The same key can be proposed
again after its task failed or its change was reverted, up to three times. An
audit should therefore name the defect in the key, not repeat its title. Story map
cards are recorded too.

## Quality baselines (`ratchet`)

A check can compare numeric outputs with the project's best value so far:

```yaml
    test:
      type: check
      run: make test-report    # writes {"test_count": 412, "lint_warnings": 3} to $AI_FLOW_OUTPUTS
      outputs: { test_count: integer, lint_warnings: integer }
      ratchet: { test_count: min, lint_warnings: max }
      outcomes: [pass, fail, regressed]
      next: { pass: open_pr, fail: fix, regressed: fix }
```

`min` values may not fall and `max` values may not rise. A regression changes
`pass` to `regressed` if the check declares that outcome, otherwise to `fail`. A
better value becomes the baseline when the run succeeds.

## Work queue: one finding per visit

When an audit or review returns several separate findings, `fix-next-finding`
fixes one per visit (a fresh session, its own test and commit) and reports
`remaining`; a switch loops back while any remain, then verification runs once:

```yaml
    fix:
      uses: preset/fix-next-finding
      next: { fixed: queue, rebutted: queue, stuck: $fail }
    queue:
      type: switch
      cases:
        - when: 'nodes.fix.outputs.remaining > 0'
          outcome: more
      default: done
      next: { more: fix, done: verify }
```

## Releases without a person

A project with a `deploy` config can merge, deploy and roll back on its own. These
settings are rules of the engine, so a flow cannot plan around them:

```yaml
spec:
  review:
    required: true               # an approving review before any merge
  deploy:
    branch: main
    pipeline: [test, release]    # only these CI jobs mean "no release was built"
    probes:                      # pages check_health requires, from the control plane
      - { url: "https://example.org/", contains: "<title>Example" }
    protected: [".github/**"]    # never merged by ai-flow
    rollback: { workflow: rollback.yml }
  notify:
    summaries: true
    digest: { at: "08:00", tz: America/Denver }
```

- `review.required`: after the last step that wrote code, a catalog preset of
  category `review` must have approved, on a model none of the authors used, and no
  review of that code may have ended otherwise. Merging or reverting the base
  branch does not need a new review. Otherwise the merge returns `blocked`.
- `deploy.protected`: a pull request that touches these paths returns `blocked`, and
  a person merges it. Protect the files that check the agents' work.
- `deploy.probes`: `check_health` fetches each page on every poll, and a page that
  fails or lacks its text for longer than the sustain window is a violation. Probe
  from here, not from a step: run pods may not reach a site in their own cluster.
- `deploy.pipeline`: without it, any failed job on the merged commit ends
  `wait_for_deploy`, which a flow answers with a rollback.

The release rules in [SPEC.md](SPEC.md) still apply: CI must pass on the exact
commit, and a run only succeeds once `check_health` reports the release healthy.

`notify.summaries` replaces the `succeeded` and `failed` events with a short text
about the result: shipped, rolled back, did not ship, a pull request that waits,
or "check production" when a merged release is neither healthy nor undone. Runs
that ship nothing appear only in the digest. `notify.digest` sends one summary a
day, unless nothing happened and nothing waits. `notify.events` overrides the
environment's events for the project.
