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
the automated assessor uses the alternate `gpt-6-luna`. The three configured
model options are `gpt-6.1-sol`, `gpt-6-luna`, and `gpt-6-astra`, routed through
`home` with matching aliases. Sol remains the default streaming planner; the
examples use Sol and Luna as an illustrative subset. Their catalog fields are
selection hints, not performance claims. Earlier live smoke tests covered GPT-6 Sol
(the previous version) and Luna using the [existing OpenCode login](CHATGPT-PROVIDER.md). The 100,000-token
catalog limit is conservative, not a full-capacity claim. Before real execution,
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
  `budget.wall` exists in the flow schema but is not currently enforced by the
  engine. These examples rely on node visit limits and explicit node/gate timeouts,
  not an assumed global deadline.

The exhaustion gate is an optional acknowledgment, not a bypass to success or a
way to reset the exhausted node. For fully automatic handling, route exhaustion
directly to `$fail` and remove the unused gate. Likewise, omit human approval
unless task/project guidance calls for it.

## What a human revision means

The current gate API records the selected outcome and who chose it; it does not
attach arbitrary reviewer prose. `revise` therefore requests another pass against
the **existing** task criteria, as stated in the example's work prompt. It does
not secretly provide new instructions or reset any limit. If a reviewer has new
requirements or needs more attempts, revise the task/flow through the normal
workflow and start a new run; saved run snapshots retain their original flow.
