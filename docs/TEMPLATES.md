# Reusable flow templates

The YAML files in [`examples/templates`](../examples/templates) are copy-and-edit
starting points using the existing `ai-flow/v1alpha1` Flow format. They expand
into ordinary nodes and transitions; they need no new subflow engine or runtime.

| Template | Pattern and terminal meaning |
| --- | --- |
| [`test-implement-verify.yaml`](../examples/templates/test-implement-verify.yaml) | Write a regression test → confirm red → implement → verify; verification failures loop back, at most three implementation visits. Success means verification passed. |
| [`investigate-propose-approve.yaml`](../examples/templates/investigate-propose-approve.yaml) | Read-only investigation → structured proposal → human approve/revise/reject; revisions return to investigation, at most three visits. Success means proposal approved, **not implemented**. |
| [`review-repair.yaml`](../examples/templates/review-repair.yaml) | Inspect an existing branch → repair findings → re-review → deterministic verification; failures return to repair, at most three repairs/four reviews. Success means review and checks passed. |

No template publishes a PR or merges a change. Add the existing
`open_pull_request` action if publication is part of your desired flow. The repo
grant's write mode allows commits to the run branch; ending at `$success` alone
does not merge them upstream.

## Offline validation

```sh
make test-templates
# Or validate one with an existing ai-flow binary:
./bin/ai-flow validate -config examples/templates/config \
  examples/templates/test-implement-verify.yaml
```

The test loads [`config/fixture.yaml`](../examples/templates/config/fixture.yaml)
and runs the actual strict flow parser, resolver, grant/model checks, reference
checks and graph validation on all three templates. It requires zero warnings
or errors. It does not launch nodes, resolve DNS, connect to models or clone repos.

**The fixture config is validation-only.** Its `.invalid` repository/image and
loopback port-1 model endpoint intentionally do not provide live resources. The
templates were format-validated, not executed against models or real repositories.

## Specialize before execution

1. **Task and project.** Copy a template, choose a unique `metadata.name`, replace
   `template-project` with your project, and provide task acceptance criteria in
   the bound task or `spec.description`. `metadata.start: manual` is deliberate.
   Remove it or choose `auto` only when that is the desired project behavior.
2. **Repository and base.** Replace `repo/template` everywhere with your catalog's
   git grant, including `spec.repo.grant` and each node's `grants`. Set the base
   branch or commit. For review-repair this must be the branch/commit you actually
   want reviewed; a new run starts its own branch from that base. A fresh run has
   no run diff, so the first review uses an **agent** to inspect the checkout.
3. **Models.** The catalog offers three aliases: `gpt-6-sol`, `gpt-6-luna`, and
   `gpt-6-astra`. Templates use an illustrative subset: Sol for coding/investigation
   and Luna for review/proposals. All three use `home` with matching upstream aliases;
   the validation fixture includes only Sol/Luna and points `home` to an inert
   endpoint. Sol remains the default streaming planner; Luna and Astra are additional
   choices. The Sol and Luna live gateway aliases were smoke-tested with existing
   OpenCode authentication; see [CHATGPT-PROVIDER.md](CHATGPT-PROVIDER.md).
   All three declare frontier size, reasoning, good tool use, and subscription cost
   as selection metadata, not performance claims; the 100,000-token working
   limit is conservative, not full capacity. Choose the smallest sufficient
   configured model based on acceptance evaluations. The proposal `llm` step
   consumes explicit evidence and cannot independently browse source. No model
   fallback is configured by default. Record confirmed upstream versions and any
   explicit per-flow fallback policy for experiments; catalog aliases and presets
   can change independently of saved flow versions.
4. **Runtimes and harness.** Replace every `fixture-go` with a catalog runtime
   containing the runner, pi harness (for agents), git, bash and the required
   build/test tools. Use a Go-enabled image for the sample `go test` commands.
   Pin an image digest for experiments. The runtime declaration alone does not
   install tools, project dependencies or permit arbitrary network access.
5. **Grants.** Give write access only to test-writing, implementation and repair
   nodes. Investigation/review/check nodes need the flow repo's read grant to
   get a checkout. Ensure project `allow.grants`/`allow.models` match. Read grants
   prevent write-back through the broker; they are not a promise that the local
   checkout is immutable. Add only actual required MCP, secret or egress grants.
   Supply credentials via Environment secret references, never template YAML.
6. **Checks.** Replace `go test ./...` with commands appropriate to your project.
   Use the same focused regression test for the red/green cycle, then add broader
   checks as needed. A red exit may also mean compilation/dependency failure;
   inspect the test-writing evidence and configure exit-code handling for your
   tools. For Go, exit 1 alone cannot distinguish those causes. Arrange dependency
   availability in the runtime or explicitly granted environment; CI validation
   of these templates does not execute their shell commands.
7. **Budgets and gates.** Tune USD/token/turn limits, timeouts and bounded
   visits to the task and project cap. There is no whole-run deadline
   (`budget.wall` is not enforced and the validator warns about it); the
   proposal flow bounds its 24-hour gate wait with the gate's own timeout. Gate outcomes are `approve`, `revise`, `reject`;
   automatic timeout fails. A revision records a decision and repeats the
   investigation; the current gate API does not attach arbitrary reviewer prose.
   Supply detailed new requirements through the task/flow editing workflow.

Validate the specialized file against **your actual config**, then save it in
the UI/API to obtain a flow version. A file's metadata version is not a substitute
for the saved version returned by the server. Explicitly choose that saved
version for each run and retain the flow/config snapshot for comparison.

## Evaluate a specialization

Use [EVALUATIONS.md](EVALUATIONS.md) to capture complete run/visit JSON and score
the saved version. Useful acceptance rules include:

- Test/implement/verify: latest `verify` outcome `pass` and output `exit_code: 0`;
  cost/token/duration thresholds; count `verify.fail` as a failure, but not the
  intentional `confirm_red.red` outcome.
- Investigation: latest `approve` outcome `approve` (or `reject` for an expected
  rejection case); define `correction_outcomes: {"approve":["revise"]}` to count
  explicit revision decisions. Approval alone does not prove proposal correctness.
- Review/repair: latest `review` outcome `approve` and latest `verify` outcome
  `pass`, plus output checks and a limit on failed verification visits.

Preserve task text, base commit, model/runtime pins and rubric between matched
comparisons. Use synthetic captures for evaluator/CI plumbing only, and label
actual exported runs `captured`. A passing flow status is not by itself evidence
that the product/task acceptance criteria were met.
