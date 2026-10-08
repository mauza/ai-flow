# Reliability and recovery

## Runs and restarts

Run/task lifecycle writes and visit transitions commit together in SQLite.
The engine reconciles persisted pending visits after startup, including gates,
switches and actions. Jobs have deterministic identities persisted before launch;
an uncertain launch response is reconciled before another launch is attempted.
Permanent configuration/authorization failures terminate the run. Transient
database/transport errors remain retryable. Pod cleanup must succeed before a
failed run releases its execution slot.

PR creation uses the existing branch lookup, including updating an existing
open PR's title/body on later action visits. A comment interrupted during
delivery fails with an explicit uncertainty message: automatic replay could
post it twice. An interrupted planner task appears as **Planning interrupted**;
use Re-plan, or repair and save its flow. Deleting a task does not prevent its
retained flows/runs from executing or completing.

Local subprocess identity is not durable across a control-plane restart.
Ambiguous local launches fail rather than risk duplicate execution. Recovery
of an external PR inherits the GitHub client's lookup of **open** PRs by branch.

## Execution settings

New runs persist their resolved flow settings and runtime image references.
Sensitive environment values are not copied into snapshots. Dependencies still
read from live configuration (such as model definitions, repository grants,
skills and endpoint identities) are fingerprinted. Changed dependencies cause
an explicit configuration-drift failure instead of silent execution changes.
The fingerprint covers only what changes execution: a model's upstream, model
id, context size, reasoning flag and `llm` defaults; a grant's access; a
skill's files. Descriptions, notes, planner hints (size, tool use, cost
label), prices and `max_concurrency` can change while runs are active.
Settings refuses an edit that would fail active runs, naming them, unless you
confirm it. Current project/catalog authorization is checked when resolving
pinned settings. Concurrency admission remains current.

Existing databases migrate automatically. Legacy runs adopt the current
configuration once, recording a `snapshot_adopted` event; their original
configuration cannot be reconstructed. Image references and model identities
do not make mutable tags/provider aliases immutable: use pinned image digests
and stable model revisions where reproducibility is required.

## Budgets and grants

Monthly project spend is an exact SQL aggregate, not a paginated run listing.
The UTC month is attributed by **run creation time**. Database/resolution errors
return retriable HTTP 503 rather than treating an unavailable budget as zero.
These remain preflight spending checks, not reservations: simultaneous requests
can overshoot a cap by their in-flight usage.

Git, LLM and MCP execution grants require the token's current active visit.
Execution expires at its deadline; result/transcript delivery retains the
engine's additional 60-second reconciliation grace. Terminal result delivery
is idempotent and does not reopen a visit. Grant validity is checked per request;
already-running upstream requests are not forcibly interrupted by that check.

## Transcripts

Runner capture is bounded to a 4 MiB prefix with explicit truncation metadata.
Individual agent event lines are capped at 1 MiB. One periodic upload runs at a
time; shutdown waits for its bounded request before uploading the final capture.
Each request has a one-minute timeout, so final upload shutdown can take up to
two minutes. The broker rejects transcript bodies larger than 64 MiB with 413.

Final captures use content-addressed object keys. Each runner includes its
acknowledged final key in the result, and the broker validates it before accepting
that result. The **first accepted result** selects the visit's immutable final
transcript; an earlier attempt's upload alone does not seal the visit. Late
uploads and duplicate results cannot replace the selected artifact. An absent
final key selects no artifact rather than another attempt's capture. Unselected
objects are retained; automatic object retention/garbage collection is not part
of this change. Deleting a flow removes its runs' objects (`runs/<id>/`), best
effort after the database rows are gone. Publication serialization assumes one control plane, consistent
with the SQLite deployment.

## Health and operations

- `/healthz`: process liveness; independent of remote providers.
- `/readyz`: bounded SQLite check and local broker-listener availability;
  returns 503 when either is unavailable. The chart uses it for readiness.
- `/api/overview`: includes exact queue counts/oldest age and grouped node
  durations/failure categories, computed with SQL aggregates under a three-second
  deadline. The Runs page displays this summary. Failure categories describe
  recorded execution facts, not inferred provider causes.

Browsers reload resource snapshots on event-stream reconnection. The editor
pins the version being run, preserves pending planner proposals across tabs,
keeps a separate external-save conflict, and rejects stale validation responses.

## Verification and rollout

Local checks:

```sh
go test ./...
go test -race ./...
go vet ./...
make helm-check
make test-templates
# In web/:
npm ci
npx playwright install chromium
npm test
npm run build
```

For an already installed Chromium, set
`PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chromium` when running `npm test`.
Browser tests exercise the actual UI at desktop/mobile sizes with deterministic
mock API responses. Go regressions use temporary databases, fake launchers and
local HTTP servers, including restart/uncertain-delivery cases.

Live Kubernetes Job retry/cleanup, real Git/LLM/MCP/S3 integration, and real-model
evaluation scores need deployment-level verification. The evaluation fixtures
are synthetic; see [EVALUATIONS.md](EVALUATIONS.md) for capturing and scoring real
runs. CI is defined in `.github/workflows/ci.yml`; a hosted Actions run happens
only after the changes are pushed.
