# Repeatable evaluations from run exports

`ai-flow-eval` is a standalone, offline Go command. It scores captured
`GET /api/runs/{id}` responses (or the equivalent `{ "run": ..., "visits": [...] }`
envelope built from store exports), and compares JSON reports by stable case ID.
It needs no database, cluster, model, API token or additional runtime dependency.

**The examples are synthetic, hand-authored fixtures. No model evaluations were
run to produce them.** They test the scoring mechanism, not model quality. The
fixture flow names do not identify saved deployments or executed template flows.

## Quick start

From the repository root:

```sh
make test-eval
make build-eval
./bin/ai-flow-eval -suite examples/evals/baseline-suite.json > /tmp/baseline.json
./bin/ai-flow-eval -suite examples/evals/candidate-suite.json \
  -baseline /tmp/baseline.json > /tmp/candidate.json
```

The first evaluation exits **0** (2/2 cases pass). The second intentionally exits
**1** (1/2 cases passes), while still writing the complete comparison. Inspect
`cases[].checks`, `cases[].nodes[].failures` and `comparison.cases[].delta`.
In the regressed case, duration increases by 10,000 ms, cost by $0.01, tokens by
500, and failed visits by two. These numbers are invented fixture data.

JSON goes to stdout; a short summary/errors go to stderr. Exit codes:

| Code | Meaning |
| --- | --- |
| 0 | All evaluation cases passed (or `-h` requested) |
| 1 | Valid captures, one or more scoring checks failed; JSON report emitted |
| 2 | Bad arguments, invalid/incomplete input, incompatible baseline or I/O error; no evaluation report |

Use the built command to consume exit codes directly. `go run` wraps a child
exit code and can obscure the distinction between 1 and 2. Write a comparison
to a **different file** from its baseline; shell redirection truncates files
before the command reads them.

## Define a suite

See [`baseline-suite.json`](../examples/evals/baseline-suite.json) for a complete
example. Suites are strict JSON: unknown fields, trailing JSON documents,
duplicate case IDs, nonpositive versions and negative limits are rejected.
Each suite needs `schema_version: 1`, `name`, `provenance` (`synthetic` or
`captured`), and a nonempty `cases` array. Do not mix synthetic and real captures
in a suite; provenance is a user declaration, not independently verified.

Each case has:

- `id`: stable identity for a task/acceptance test across experiments.
- `capture`: export filename, relative to the **suite file**, or an absolute path.
- `flow_name`, `flow_version`: required identity and positive, explicit saved
  version; latest/version 0 is forbidden. A mismatch is an input error.
- `task_id`: optional additional identity check on the captured run.
- `description`: optional context; no effect on scoring.
- `expect`: required scoring rules. `status` must explicitly be `succeeded`,
  `failed` or `canceled`; there is no implicit success default.

Optional `expect` fields:

| Rule | Meaning |
| --- | --- |
| `max_duration_ms` | Inclusive wall-duration limit, from run start to finish |
| `max_cost_usd` | Inclusive limit on the run's reported cost |
| `max_tokens` | Inclusive limit on the run's reported total tokens |
| `max_failed_visits` | Inclusive limit on failures, including recovered attempts |
| `failure_outcomes` | Node → outcome list also classified as failures, e.g. `{"verify":["fail"]}` |
| `correction_outcomes` | Gate node → recorded outcomes explicitly treated as human corrections, e.g. `{"approve":["revise"]}` |
| `max_human_corrections` | Inclusive correction limit; requires `correction_outcomes` |
| `nodes` | Assertions on each named node's latest visit by highest `seq` |

A node assertion requires a unique `node` and at least an `outcome` or `outputs`
object. The latest visit must have status `succeeded`; an earlier passing visit
cannot hide a later error. `outputs` compares the specified top-level fields by
JSON value. Nested objects compare fully (object key order ignored), arrays are
order-sensitive, numeric values compare exactly without float rounding, and
missing fields differ from explicit `null`. Other output fields are ignored.

Example assertion:

```json
{"node":"verify","outcome":"pass","outputs":{"exit_code":0}}
```

There is no implicit interpretation of output prose and no model-based judge.
Choose acceptance checks that actually establish your task's requirements;
status-only rubrics measure only orchestration completion.

## Scoring versus run status

Every enabled check must pass for a case to pass. The suite reports both the
fraction of passing evaluation cases (`eval_pass_rate`) and the fraction of
captures with `run.status == "succeeded"` (`run_success_rate`). These are different:

- A run can reach `$success` even if a faulty flow routes a failed check there.
  Explicit output/outcome assertions detect this (the candidate fixture).
- A rejected proposal may correctly end at `$fail`. A case expecting rejection
  can pass even though its run failed (the proposal fixture).
- Visit status `succeeded` means the node executed and returned an outcome. A
  check's `fail` outcome or review's `changes` outcome is not a runtime error.

Failure counts include **every** visit with status `error` or `canceled`, a
nonempty error, or a matching `failure_outcomes` entry. A visit satisfying more
than one criterion counts once. Configure only meaningful business failures:
the intentional red test in test-implement-verify usually should not count.
Reports list all visited nodes, their visit counts and each failed sequence.
The run's error is also preserved separately.

## Measurement semantics

- Duration is `finished_at - started_at` in milliseconds, including gates and
  retries but excluding pre-start queue time. Missing/zero endpoints produce
  `null`; reversed nonzero timestamps are invalid.
- Cost and tokens come from the **run totals**, without adding visit totals
  again. Absent/null totals remain `null`, not zero. A threshold against an
  unknown metric fails. Reported zero cost does not prove free execution:
  providers/local deployments may not report or configure pricing.
- `gate_decisions` counts completed gate visits with nonempty `decided_by`,
  excluding the engine's automatic `"timeout"` actor. It is a count of recorded
  decisions, not verified human identities.
- `human_corrections` is `null` unless the rubric explicitly supplies
  `correction_outcomes`. When supplied, it counts those recorded gate decisions
  whose node and outcome match. All visits count, including repeated revisions.
  Gate timeouts, agent retries and mere rejection are not implicitly corrections.
  A zero means no matching recorded decisions in this capture. It does **not**
  mean no human edits happened elsewhere. Gate decisions submitted by automation
  with an ordinary actor label cannot be distinguished from a human by this API.
- There is no supported count of human-edited lines, correction effort/time,
  semantic correctness or reviewer satisfaction in these exports. The evaluator
  does not infer such metrics from free text or invent missing data.

Only terminal runs and terminal visits are accepted. Missing/null `visits` is an
input error; `[]` is accepted. Sequences must be unique and positive, and each
visit must belong to the run and name its node/type. Additional API fields such as graph, task, YAML
and events are tolerated. The evaluator cannot detect an intentionally omitted
visit or authenticate a capture; export the complete response.

## Capture actual runs and pin the experiment

Use a development instance you control. The following are **manual examples**,
not operations performed by the evaluator:

```sh
# Start an already saved flow at a specific version (never omit version).
curl --fail-with-body -sS -H 'Content-Type: application/json' \
  -d '{"version":3}' http://localhost:8080/api/flows/my-flow/runs

# After it finishes, export the full response; replace RUN_ID with the returned id.
curl --fail-with-body -sS http://localhost:8080/api/runs/RUN_ID > run.json
```

For authenticated instances, supply the configured bearer token through your
normal local credential handling. Keep credentials out of fixtures and reports.
Do not point this example at production as part of CI. Save the pinned flow YAML
from the response or `GET /api/flows/my-flow/versions/3` alongside the experiment.

The CLI deliberately implements **export scoring only**: no HTTP submission,
polling, task planning or gate decisions. Submit through the existing UI/API,
wait for `succeeded`, `failed` or `canceled`, then capture. Set suite provenance
to `captured` and replace every synthetic case identity/path.

Flow versions are scoped to their store and do not alone pin a whole experiment.
Record task text/acceptance criteria, repository commit/base SHA, actual model
identifiers and provider settings, catalog and resolved presets, fallback policy,
runtime image digest, harness/tool versions, grants, budgets and sample IDs.
Avoid moving model aliases and image tags. Changing catalog settings can change
new runs even when saved flow YAML is unchanged. Save the execution settings
available in your deployment along with the exports. Seed/temperature controls
and deterministic model execution are not supplied by this evaluator.

## Baseline comparisons and repeatability

Generate a baseline report with this command/schema, retain its suite and raw
captures, then use `-baseline`. Cases pair by stable ID regardless of array order.
The schema, suite name, provenance, case set, flow names and normalized scoring
rules must match. Threshold/assertion changes require a new baseline; they are
not silently treated as improvements. Different pinned flow versions are allowed
and reported explicitly. Task IDs may differ for repeated submissions of the
same logical task; keeping the task/acceptance criteria equivalent is your job.

Every report includes raw capture SHA-256 and rubric SHA-256 per case. Comparison
includes the baseline file SHA-256 and current-minus-baseline metric deltas;
unknown on either side stays `null`. Negative cost/duration/token/correction
deltas mean lower observed usage, not automatically better quality. A regression
is precisely baseline pass → current fail. Exit status still follows current
case scoring; there is no hidden percentage-cost regression threshold.

Reports have no generated timestamp, and equivalent inputs produce deterministic
bytes (including stable node/check order). The hashes identify inputs, not their
authenticity. Reports/captures can contain task outputs and errors; choose what
you retain/share accordingly. Baseline reports are trusted local artifacts.

For repeated model trials, give each sample a stable case ID, retain all samples,
and compare matched sample sets. This lean tool supplies per-case metrics and
unweighted pass rates, not statistical confidence, significance testing, aggregate
cost extrapolations or a claim that a single sample predicts model performance.

## Verification and CI

`make test-eval` covers API/store encoding compatibility, scoring versus status,
retries/latest visits, unknown metrics, exact JSON equality, correction counting,
input rejection, baseline compatibility, deterministic output and CLI exit codes.
It also validates the reusable templates with the real config/resolver, offline.

CI runs Go tests/race/vet, frontend install/build and tests when the UI provides
an npm `test` script, default/kind Helm lint/render with both Garage secret modes,
and tracked-file/whitespace checks. It uploads explicitly **synthetic** example
reports and asserts that the deliberately regressed candidate exits 1. CI does
not run model workloads or use production credentials.
