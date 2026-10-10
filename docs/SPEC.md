# ai-flow — Task Flows as State Machines

Status: implemented · updated 2026-10-08

## 1. What it is

Every task gets its own small state machine — a **flow**. A model (the planner)
drafts the flow; each node is one narrow step: a single LLM call, a coding-agent
session (pi), a deterministic check, a human gate, a CEL switch, or a built-in
action. Steps are focused and verifiable, using the smallest sufficient configured
model from the catalog. Flows are
YAML; the UI shows them as a graph you can edit by hand or by chatting with the
planner. Runs execute each pod step as a Kubernetes Job with exactly the tools,
skills, MCP tools, models and repo access that step declares — and no raw
credentials: git, LLM and MCP traffic all go through the control plane, which
holds the secrets and re-checks every request against the step's grant.

Everything runs in one local kind cluster (`make dev-up`) or any other cluster
with the same Helm chart. External services (a home LiteLLM, R2, GitHub, Linear)
are plain URLs in config.

## 2. Concepts

| Term | Meaning |
|---|---|
| **Task** | Unit of intent: from Linear (label + state trigger) or created in the UI. |
| **Flow** | Versioned YAML state machine for one task. Every save is a new version; runs pin one. |
| **Node** | One step. Emits exactly one **outcome** from its enum, plus typed **outputs**. |
| **Transition** | `outcome → next node` (or `$success` / `$fail`). Only enum outcomes route; free text never does. |
| **Visit** | One execution of a node in a run. Loops create `fix#1`, `fix#2`, … |
| **Catalog** | The menu: models, runtimes (images), grants, skills, presets, planner settings. |
| **Grant** | A named capability: git repo (read/write), MCP server + tool subset, k8s secret, egress. |
| **Project** | Repo, allowed grants/models, budgets, start mode, planner guidance, Linear link, deploy settings. |
| **Story map** | A product's user journey (phases → activities → user tasks, sliced into releases) kept in its repo's `product/`; parts of it become tasks. See [PRODUCT.md](PRODUCT.md). |

## 3. Architecture

```mermaid
flowchart LR
  L[Linear] -- poll/webhook --> CP
  UI[Browser UI] -- :8080 API + SSE --> CP
  subgraph CP[control plane: one Go binary]
    API[API + UI] --- ENG[Engine: state machine]
    ENG --- PL[Planner]
    BR[Broker :8081] --- GP[git proxy]
    BR --- LP[LLM proxy]
    BR --- MP[MCP calls]
  end
  ENG -- create Job --> K8s[(runs namespace)]
  K8s --> POD[node pod: node-runner + pi]
  POD -- the only allowed egress --> BR
  GP --> GH[(GitHub)]
  LP --> UP[(OpenAI-compatible upstreams)]
  MP --> MCPS[(MCP servers)]
  BR --> S3[(object store: Garage / S3 / local disk)]
```

Components (all in `ai-flow`, one binary and image, plus the agent runtime image):

| Component | Package | Job |
|---|---|---|
| Engine | `internal/engine` | Interprets runs: one active node at a time, visits, max_visits, gates, switch, actions, retries of crashed pods, concurrency limit. |
| Launcher | `internal/launcher` | Kubernetes Jobs (prod/kind) or child processes (`-local`). |
| Broker | `internal/broker` | Pod-facing port: token exchange, results, progress, transcripts, LLM proxy, git proxy, MCP calls. |
| Node-runner | `internal/runner` | Pod entrypoint: exchange, clone via proxy, run node, commit + push, report. |
| Limit shim | `internal/runner/shim.go` | In-pod OpenAI-compatible proxy applying the step's `on_limit` policy. |
| pi extension | `pi-ext/index.ts` | `flow_finish` tool + granted MCP tools as native pi tools. |
| Planner | `internal/planner` | Task → flow YAML with a validator repair loop; chat revisions. |
| Validator | `internal/resolve` | Presets/defaults resolution, validation, graph projection. Shared by planner, UI, CLI, engine. |
| Intake | `internal/intake` | Linear polling, status + comment mirroring. |
| App | `internal/app` | Use cases shared by the API and intake: tasks and planning, live config edits, repository linking, sending story-map work to flows, the map assistant, run reviews. |
| Workspace | `internal/workspace` | Scratch checkouts of each project's `product/` through the GitHub API; one-commit pushes that never force. |
| Story maps | `internal/storymap` | Map and user-task files: load, validate, scope and brief. |
| Store | `internal/store` | SQLite: tasks, flow versions, runs, visits, events, chat, and a kv table (stored catalog and projects, story-map scopes, chats, reviews). |
| UI | `web/` | React + React Flow + ELK + CodeMirror, embedded in the binary. |

### Off the shelf

pi (agent harness), Kubernetes Jobs + NetworkPolicy + TokenReview, Garage (object
store), any OpenAI-compatible model server (LiteLLM, llama.cpp, vLLM, Ollama…),
the MCP Go SDK, CEL (`cel-go`), React Flow + ELK.js, CodeMirror, the `yaml` library.

## 4. Flow format

```yaml
apiVersion: ai-flow/v1alpha1
kind: Flow
metadata:
  name: fix-slugify-digits          # lowercase-dashes
  project: sandbox
  task: { source: linear, id: ENG-22 }
  start: manual                     # optional: overrides the project's start mode
spec:
  description: Keep digits in slugify output.
  repo: { grant: repo/ai-flow-sandbox, base: main }   # optional: defaults to the project repo
  budget: { usd: 4.00 }
  defaults: { runtime: agent-base, timeout: 20m }      # optional per-flow node defaults
  start: reproduce
  nodes:
    <id>:                           # lowercase_snake_case
      type: llm | agent | check | gate | switch | action
      uses: preset/<name>           # node fields override the preset
      description: why this step exists
      model: <catalog model>        # shorthand for llm.model
      llm:                          # full model config, §4.3
      harness: pi                   # agent nodes (default pi)
      runtime: <catalog runtime>
      grants: [repo/x:write, mcp/y]
      skills: [small-diffs]
      inputs: { test: "${{ nodes.reproduce.outputs.test_name }}" }
      prompt: |                     # llm/agent instructions; gate question
      outputs: { summary: string, files: [string], count: number }
      outcomes: [done, stuck]
      next: { done: test, stuck: $fail }
      max_visits: 3                 # required on some node of every loop (gates count too)
      on_exhausted: escalate        # default $fail
      timeout: 30m                  # pod nodes; on gates it adds a `timeout` outcome
      retry: { limit: 1 }           # infra retries of the pod (Job backoffLimit)
      run: python -m unittest -v    # check
      exit_codes: { "0": pass, default: fail }   # check (this is the default)
      cases: [{ when: "run.diff.files_changed > 15", outcome: big }]   # switch
      default: small                # switch
      action: open_pull_request     # action: see NODE-CATALOG.md (PRs, merge, CI, deploy, health, rollback)
      with: { title: "${{ task.title }}", body: "...", draft: false }
```

### 4.1 Node types

| Type | Runs where | Outcome from |
|---|---|---|
| `llm` | pod | One chat completion with a JSON-schema response format (`{outcome, summary, outputs}`); the branch diff is included. |
| `agent` | pod | pi session in the repo; ends with the `flow_finish` tool. One nudge if the agent forgets. |
| `check` | pod | `bash -o pipefail -c <run>`; exit code → `exit_codes`. Outputs `exit_code`, `log_tail`, plus any declared `outputs` the command writes as one JSON object to `$AI_FLOW_OUTPUTS` (type-checked; undeclared fields fail the step; no file means none). Rendered `inputs` arrive as `$AI_FLOW_INPUT_<NAME>` (never spliced into the command text). |
| `gate` | control plane | A human picks an outcome in the UI, with an optional note that becomes `outputs.note` (the next step sees it in its context); optional timeout. |
| `switch` | control plane | First matching CEL case, else `default`. |
| `action` | control plane | Built-in: `open_pull_request` (idempotent; PR body includes a step table), `comment_task`, and the waiting actions `merge_pull_request`, `wait_for_checks`, `wait_for_deploy`, `check_health`, `rollback_deploy`, which the engine polls until they finish or time out (`timeout` outcome). [NODE-CATALOG.md](NODE-CATALOG.md) has their outcomes and outputs. |
| `parallel` | control plane | Starts every node in `branches` at once; emits `joined` when all branches have reached its `join`. |
| `join` | control plane | Runs after every branch of its parallel node arrived; routes like a switch over any branch's results (no cases → `done`). |

Implicit outcomes: `timeout` on gates with a timeout; `limit` on llm/agent nodes
whose `on_limit` uses `action: outcome`; `joined` on parallel nodes.

#### 4.1.1 Parallel branches

```yaml
checks:
  type: parallel
  branches: [lint, test, review]   # the first node of each branch
  join: gather
lint:   { uses: preset/custom-check, run: go vet ./..., next: { pass: gather, fail: gather } }
test:   { uses: preset/go-test, next: { pass: gather, fail: diagnose } }
diagnose: { uses: preset/investigate-bug, model: gpt-6.1-sol, next: { diagnosed: gather, inconclusive: gather } }
review: { uses: preset/code-review, model: gpt-6-luna, next: { approve: gather, changes: gather } }
gather:
  type: join
  cases: [{ when: 'nodes.test.outcome == "pass" && nodes.review.outcome == "approve"', outcome: green }]
  default: red
  next: { green: open_pr, red: fix }
```

A branch is every node reachable from its first node without passing the join;
it can be several steps long and loop (bounded by `max_visits`). The validator
enforces the rules that make concurrent branches safe and readable:

- branches are **read-only**: no `repo:write` grant, because branches share the
  run branch and would race to push; edits go after the join;
- branches hold only `llm`, `agent`, `check` and `switch` nodes (no gates,
  actions or nested parallel nodes);
- a branch may route to `$fail` (failing the run) but not `$success`;
- nothing outside a branch routes into it, only its branches route to the join,
  and no node belongs to two branches.

Branch visits record their parallel visit (`fork_seq`) and branch (`lane`). A
branch step's context shows the main path and its own branch, not its siblings;
the join and everything after it see every branch's results. Any branch error
fails the run and stops the other branches' Jobs. Branch pods count against
cluster capacity, not `runs.maxConcurrent`, which limits runs.

### 4.2 Templates and context

`${{ path ?? "fallback" }}` with roots `task`, `run` (`id`, `branch`, `base`,
`diff.files_changed|lines_added|lines_removed|lines_changed`, `last_node`),
`nodes.<id>.{outcome,summary,visit,outputs.<f>}` (latest visit),
`nodes.<id>.history` (every successful visit, oldest first) and `inputs`. Rendered
once, in the control plane, so model output is never re-evaluated. CEL switch
expressions see the same values.

Every pod node also receives a context section: the task, a list of earlier steps
with outcomes and summaries, every pass of each step that repeated (with outputs), the previous step's outputs and log tail, its inputs,
and the branch diff stats.

### 4.3 Model config and limits per step

```yaml
llm:
  model: gpt-6.1-sol
  thinking: medium
  limits: { tokens: 400000, usd: 0.50, turns: 80 }
  on_limit:
    rate_limited:     { action: retry, max: 5, backoff: 20s, then: fail }
    quota_exhausted:  { action: wait, max_wait: 2h, then: fail }
    context_exceeded: { action: fail }
    budget_exceeded:  { action: outcome }     # emits `limit`; route it in next
```

The checked-in catalog has no model fallback. A flow may explicitly opt into
`fallbacks: [gpt-6-luna]` and a corresponding `fallback` limit action if its project
allows Luna; this is a per-flow policy, not an automatic catalog default.

Precedence: node → preset → flow defaults → project defaults → the catalog
model's own `llm` → catalog defaults → built-ins. A model entry can therefore
give a free local model a larger token budget than the catalog-wide default;
`limits` merge field by field (raising `tokens` keeps the default `turns`).

Actions: `retry` (exponential backoff, honours `Retry-After`), `wait` (until the
upstream's reset time, capped by `max_wait`), `fallback` (next model), `outcome`
(stop, emit `limit`), `fail` (step error → run fails). `then` chains. Layering:
node → preset → flow/project/catalog defaults → catalog model → built-in defaults
(rate_limited: retry ×5 then fail; quota: wait 30m then fail; others: fail).

`thinking` (`off`, `minimal`, `low`, `medium`, `high`, `xhigh`) reaches the
endpoint only for agent nodes whose model sets `reasoning: true` and a
`thinking_format`, the parameter that endpoint understands: `reasoning_effort`
(OpenAI and most gateways), `qwen-chat-template` (Qwen on vLLM:
`chat_template_kwargs.enable_thinking`), `qwen`, `deepseek`, `zai` or
`openrouter`. With a format and no level, pi asks for `medium`; without a
format nothing is sent and the endpoint's own default applies. A model can set
its default level in its own `llm` block. When the shim falls back to a model
with another format, it drops the thinking parameters (that model's default
applies) and lowers the reply cap to the fallback's `max_output_tokens`.

Two enforcement points: the **limit shim** in the pod (behaviour: retries,
waits, fallbacks, token/turn limits) and the **LLM proxy** in the control plane
(security: model allowlist from the grant, node `limits.usd`, flow `budget.usd`,
project per-run and per-month caps, usage accounting, and each catalog model's
`max_concurrency`: calls beyond it wait for a slot, across all pods and parallel
branches). Gateway 500s that report an unreachable backend (LiteLLM's
`APIConnectionError`) count as `rate_limited`, so retries and fallbacks apply.

## 5. Configuration

A config directory of YAML documents, each with a `kind`; later files override
earlier `Environment`/`Catalog` documents (`-config deploy/config -config local.yaml`).
Secrets are env vars named by `*Env` fields, never inlined.

- **Environment** — where things run: listen ports, public URL, pod URL, runs
  namespace, concurrency, LLM upstreams (`baseUrl`, optional `baseUrlEnv` override, `apiKeyEnv`), object store
  (`garage` | `s3` | `local`), git host tokens, GitHub API, Linear connection,
  MCP servers (URL + headers), notifications (`notify`: ntfy server, topic,
  token, events, `stuckAfter`).
- **Catalog** — models (upstream + model id + planner metadata + default `llm`,
  `thinking_format`, `max_output_tokens`), harnesses (`instructions`,
  `settings`, see §7.2.1), runtimes, grants, skills (inline files or a directory), presets,
  node defaults, planner (model, stream, guidance, attempts).
- **Project** — repo grant, base branch, `start: manual|auto`, allow-lists for
  grants and models (glob patterns), budgets, planner guidance, agent
  instructions (`agent.instructions`, for every agent step), Linear link:
  team, optional project filter, trigger label + states, state mapping, comments;
  and `deploy` when merging to a branch ships the project: version endpoints,
  health queries and URL, an optional rollback workflow and Argo CD app.

**Where it lives at runtime.** On its first start the server copies the
catalog and projects into its database and serves them from there; the UI's
Settings page edits them, each change checked against the whole config and
applied live. On later starts, every entry that changed in the files since the
previous start (a release bumping a runtime image, a synced preset) replaces
that entry in the database. The environment always comes from the files.

The kind setup lives in `deploy/config`; `deploy/local/environment.yaml` layers
local-process mode on top.

The checked-in catalog offers `gpt-6.1-sol` (the streaming default planner),
`gpt-6-luna` and `gpt-6-astra` on the `home` upstream, which points at OpenAI
unless `LLM_BASE_URL` names another OpenAI-compatible endpoint (`baseUrlEnv`).
Their `size`, `tool_use`, `cost` and `context_tokens` are selection metadata for
the planner, not performance claims; deployments add their own models.

## 6. Planning

1. A task arrives (Linear poll or UI). The planner gets: rules and the flow format,
   the catalog filtered by the project's allow-lists, global + project guidance
   (how much the human wants to be in the loop), an example flow, and repo context
   from GitHub (file tree, AGENTS.md, CLAUDE.md, README.md).
2. The reply's YAML block is parsed, identity fields are pinned, the flow is
   re-rendered in a stable layout and validated. Errors go back to the model
   (up to `planner.max_attempts`).
3. The result is saved as a version (even with errors, so a human can fix it) and
   the explanation is stored as the first chat message. With `start: auto` a valid
   flow runs immediately.
4. Chat revisions send the current editor YAML, validation findings and recent
   chat history; the UI shows the proposal as a diff to apply, then you save.

No gate is mandatory. The guidance text decides when the planner adds gates, and
it must justify them in the gate's `description`.

The planner also picks a rigor level and names it in its explanation: light
(implement → check → PR), test-first (the default for behavior changes),
investigate-first (a read-only exploration step when the plan hinges on an
unknown), or independent review (a non-author review, on a different model when
one is allowed, when a plausible change could fail in ways its author would not
see). Importance alone never escalates; an open unknown or an author-blind
failure mode does. The rungs follow openrig's planning dial.

Validation (planner, UI, CLI, engine): schema, ids, start, every outcome routed,
no stray `next` keys, targets exist, reachability, every node can reach a
terminal, every cycle bounded (`max_visits` or a gate), models/runtimes/harnesses/
skills/presets/actions exist, grants and models allowed by the project, CEL
compiles, template references resolve, output types valid, budget ≤ project cap.
When the project's `deploy.branch` is the flow's base (merging deploys), every
`merge_pull_request` must follow a passing `wait_for_checks` with no repo-writing
step in between, and after `merged` the run can only succeed through
`wait_for_deploy` (deployed) and then `check_health` (healthy). A
`rollback_deploy` node needs the project's `deploy.rollback` workflow.
Fields that parse but that nothing enforces (a `model` on a switch, `skills` on
an llm node, `thinking` on a model without `reasoning: true` or `thinking_format`,
`spec.budget.wall`, …) produce
a *declared but not enforced* warning rather than silently looking like they work.

## 7. Execution

### 7.1 Engine

A run is a pinned flow version with one active node on its main path; a
parallel node is that active node while its branches each advance one visit at
a time. The engine loop (every 3s and on demand) starts queued runs up to
`runs.maxConcurrent`, then for each active run looks at its latest visit (and,
under a parallel node, at the latest visit of each branch):

- finished → follow `next[outcome]`; entering a node that already ran
  `max_visits` times goes to `on_exhausted` instead;
- a pod node whose Job ended without a result → error after a grace period;
- a gate past its deadline → `timeout` outcome.

Resolved settings and runtime image references are also pinned. Restart recovery
reconciles pending visits and uncertain launches using durable transition state.
See [RELIABILITY.md](RELIABILITY.md) for configuration drift, legacy migration,
action replay, budget and transcript semantics.

Transitions happen only on the engine's loop; the broker and API record facts
(a result arrived, a gate was decided) and wake it. Infra errors (pod crash,
OOM, timeout) fail the run after Job retries; business failures are outcomes.

**Resume.** A failed or canceled run can be resumed at any node (default: the
node of its last visit) with an optional note: `POST /api/runs/{id}/resume`
`{node, note}`, or **Resume** in the run view. The run keeps its branch, pinned
settings, visits and spend; the run, its task and a new pending visit commit in
one transaction, so a repeated resume is rejected rather than doubled. Visits
before the resume stop counting toward `max_visits`, giving each resume one fresh
bounded window. Steps after the resume get a *Resumed* context section (where it
stopped, the note) and `run.resumes` / `run.resume_note` in templates. A resume
needs a free run slot and fails on configuration drift like any pinned run.

### 7.2 Node pod

Job in the runs namespace: non-root, read-only root filesystem, dropped
capabilities, RuntimeDefault seccomp, no service-account token automount, `/work`
and `/tmp` emptyDirs, CPU/memory limits, `activeDeadlineSeconds`, TTL cleanup.
Only a projected token with the `ai-flow` audience is mounted.

`ai-flow node`:

1. **Exchange** — POST the projected token. The broker runs a TokenReview, reads
   the pod's labels (run, visit), checks the visit is the current one, and
   returns the bundle: rendered prompt and context, outcomes, result schema,
   repo access, model list + `llm` config, allowed tools, MCP tool schemas,
   skills, check command, and a grant JWT scoped to this visit.
2. **Workspace** — clone through the git proxy (grant passed per command, never
   written to `.git/config`), check out the run branch (`ai-flow/<flow>-<run>`)
   or create it from base.
3. **Run** — agent: pi with an isolated config dir (`PI_CODING_AGENT_DIR`), a
   custom provider pointing at the limit shim, `--no-extensions -e <ai-flow ext>`,
   `--no-skills --skill <granted and repository skills>`, `--tools <allowlist>`,
   JSON event stream as transcript and progress (setup in §7.2.1). llm: one structured call through the shim. check:
   bash with a minimal environment (no grant).
4. **Collect** — validate the outcome, commit and push if the node has
   `repo:write`, compute diff stats, upload the transcript, post the result.

#### 7.2.1 Agent harness setup

What pi sees in an agent step, from the most general to the most specific:

| Layer | Source | How pi gets it |
|---|---|---|
| Harness instructions | catalog `harnesses.pi.instructions` | `AGENTS.md` in pi's config dir (its global context file) |
| Project instructions | project `agent.instructions` | the same file, under `# Project instructions` |
| Repository instructions | `AGENTS.md` / `CLAUDE.md` in the checkout | pi's own discovery from the working directory |
| Repository `.pi/` | `.pi/settings.json`, `.pi/SYSTEM.md` in the checkout | pi's own discovery; the repository is trusted through its grant |
| Step | node prompt, outcomes, outputs, run context | `--append-system-prompt` and the prompt |
| Skills | the node's catalog `skills`, plus the repository's `.agents/skills/<name>/SKILL.md` and `.claude/skills/<name>/SKILL.md` | `--skill <dir>`; a catalog skill wins a name clash, then `.agents` over `.claude` |

**Settings.** The runner writes pi's `settings.json` with quiet startup and
retries off: the limit shim owns retries and fallbacks (the step's `on_limit`),
and every request counts as a turn, so pi and its HTTP client never retry on
their own. `harnesses.pi.settings` merges over that, object by object (for
example `compaction: {keepRecentTokens: 40000}`).

**Models.** pi's model entry takes the catalog model's `context_tokens`
(default 100000), `max_output_tokens` (default 16384) and `thinking_format`
(§4.3).

**Tools.** `read`, `bash`, `edit`, `write`, `grep`, `find`, `ls`, plus
`flow_finish` and granted MCP tools. A step without `repo:write` has no `edit`
or `write`.

Harness instructions and settings and a model's `thinking_format` and
`max_output_tokens` are execution settings: changing them fails active runs
that use them as configuration drift (the UI asks first). Project instructions
are pinned with the run's snapshot, so edits apply to new runs.

### 7.3 Access control

| Resource | What the pod holds | Enforced by |
|---|---|---|
| Git | grant JWT | Git proxy: repo must match the grant; pushes need `:write` and may only update `refs/heads/<run-branch>` (no deletes, no other refs); rejected pushes answer with git's report-status so the client prints why. Upstream token injected by the proxy. |
| Models | grant JWT | LLM proxy: model ∈ grant (node model + fallbacks), budgets, upstream key injected, usage recorded per visit and run. |
| MCP | grant JWT | Broker calls the tool only if `server/tool` ∈ grant; upstream credentials live in the environment config. |
| Secrets | the one mounted key | Only secrets named by the node's `secret` grants and allowed by the project. |
| Network | nothing | NetworkPolicy: node pods may reach DNS and the control plane's pod port (8081), nothing else. `egress` grants with `allow: internet` open public internet (private ranges still blocked). |
| Kubernetes API | nothing | No automounted token; the runner service account has no RBAC. |

Every pod node can read the flow's repo; writing requires `repo:write`.
A pod can re-exchange its own token — which only yields its own visit's grant.

`examples/redteam.yaml` probes all of this from inside a pod (16 checks).

## 8. State between nodes

| Kind | Where |
|---|---|
| Outcome, outputs, summary | Visit rows (SQLite); templates and context read the latest visit per node. |
| Code | The run branch, pushed after each writing node. Diff stats flow into `run.diff`. |
| Transcripts, check output | Object store; the accepted result pins its validated final content-addressed key, shown in the UI. Capture is bounded with explicit truncation metadata. |

## 9. Intake (Linear)

Polls each linked project (poll mode needs no public URL). In webhook mode,
`POST /webhooks/linear` (HMAC-verified) triggers an immediate poll and polling
drops to every 5 minutes as a safety net. An issue in a trigger
state with the trigger label becomes a task and is planned. ai-flow moves the
issue through the mapped states (planning → flow ready → running → succeeded /
failed) and comments with the flow link, then the PR link or the failure reason.
Moving a failed issue back to a trigger state re-plans it.

## 9a. Notifications

With `notify.ntfy` set, the engine pushes a short message (title, the gate
question or failure, a link to the run) when a gate opens, when a gate is
still waiting after `stuckAfter` (once per gate visit, remembered across
restarts), and when a run fails; `succeeded` is opt-in. Delivery is best
effort and never affects the run.

## 10. UI

- **Board** — tasks by stage (planning, ready, running, done, failed), new task,
  task drawer (description, runs, activity, re-plan, run).
- **Flow editor** — graph (ELK layered layout, orthogonal edges, loops, terminal
  pills, validation badges); side panel with the node inspector (model, prompt,
  access, skills, outcome → next, loops, effective settings), YAML editor with
  inline diagnostics, planner chat with diff proposals, and runs. Drag from an
  outcome to a node to route it. Versions, save (Ctrl+S), revert, run, delete.
  Deleting (`DELETE /api/flows/{name}`) removes every version, the planner chat,
  all runs with their visits, events and transcripts, and unlinks tasks; it is
  refused with 409 while any run of the flow is queued, running or waiting.
- **Run view** — the same graph colored live over SSE (running, done, waiting,
  failed, not reached; taken edges in green; visit counts), gate decisions,
  step timeline with progress, and step detail (result, outputs, rendered prompt,
  transcript with tool calls).
- **Catalog** — the node library and a reference of models, grants, skills,
  runtimes and projects.
- **Settings** — edit every catalog entry and project as YAML; Connections shows
  which tokens and endpoints are set without revealing them.
- **Products** — linked repositories (link from your GitHub repositories); per
  product, Markdown docs and user story maps from its `product/` directory, a
  scratch checkout with pull, diff review, discard and commit & push, a map
  grid with a task panel, sending work to flows, metrics and an assistant.
- **Run review** — on the run view, an LLM review of the run's record with
  suggestions for the flow, presets, prompts, models, config or primitives.

## 11. Environments

`deploy/chart` installs the control plane, the runs namespace with its service
account and NetworkPolicies, RBAC (TokenReview; Jobs/pods in the runs namespace),
Garage (bootstrapped by the control plane through its admin API), and optionally
the demo MCP server. Config files go in with `--set-file` or an existing ConfigMap;
secrets via an existing Secret (`ai-flow-secrets`).

- **kind** — `make dev-up` / `dev-reload` / `dev-down` (step by step in
  [LOCAL-KIND.md](LOCAL-KIND.md)). Images built on the host and
  loaded; UI at http://localhost:8080. State (SQLite, Garage) lives in a host
  folder (`STATE_DIR`, default `~/.local/share/ai-flow/<cluster>`) mounted into the
  node and used through `hostPath` volumes, with pods running as your uid, so it
  survives `dev-down`; `make dev-reset` removes it. kind's kindnet enforces NetworkPolicy a few
  seconds after a pod starts; use Calico/Cilium where that gap matters.
- **local processes** — `make dev-local`: no cluster, no isolation; for fast iteration.
- **prod** — same chart with real values: images from a registry, sealed secrets,
  ingress, `linear.mode: webhook` optional, S3/R2, Garage or the local data volume (`objectStore.type: local`).

## 12. Changes from the draft spec

| Draft | Built | Why |
|---|---|---|
| Compile flows to Argo Workflows (unroll loops into a DAG) | Own state-machine engine + plain Jobs | A flow has one active node at a time, so an interpreter is ~500 lines, handles loops natively, and removes a cluster-wide CRD install. Argo's value (DAG, artifacts, UI) mostly didn't apply. |
| Per-node LiteLLM virtual keys | LLM proxy in the control plane (grant = model allowlist + budgets) | Works with any OpenAI-compatible endpoint and needs no LiteLLM + Postgres; LiteLLM is still a fine upstream. |
| agentgateway for MCP | Broker calls MCP tools with the official Go SDK | One auth model (the grant) for git, LLM and MCP; no extra component to configure. |
| Flows in a git repo | Versioned YAML in SQLite, editable and exportable | Easier setup; every save is still a version and runs pin one. |
| `plan-task` meta-flow | Planner in the control plane with repo context from GitHub | Interactive latency for chat revisions; exploration by an agent can be added as a preset later. |
| Gates as Argo suspend | Gates handled by the engine | No pod waiting for a human. |

## 13. Not done yet

- `map` (fan-out over a list) and sub-flow nodes; parallel branches that write
  to the repo.
- Claude Code / opencode harnesses.
- GitLab forge (Git hosting is GitHub only, including product workspaces).
- Egress domain allowlists (today: none or public internet).
- Planner exploration by an agent; multi-repo flows.
- Auth for the UI beyond an optional shared token (`server.authTokenEnv`); no
  per-user identity, so product commits are authored as ai-flow.
