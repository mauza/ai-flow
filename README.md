# ai-flow

Every task gets its own state machine. A planner model turns a task (from Linear
or the UI) into a **flow** — small steps like *write a failing test → fix it →
run the tests → review the diff → open a PR*, with loops and human gates where
they make sense. Each step runs as an isolated Kubernetes Job with only the
model, tools, MCP tools and repo access it declares; git, LLM and MCP traffic go
through the control plane, so pods never hold credentials.

Small steps keep work focused and verifiable. The only configured model options
are **GPT-6.1 Sol** (`gpt-6.1-sol`), **GPT-6 Luna** (`gpt-6-luna`), and **GPT-6 Astra**
(`gpt-6-astra`). Sol remains the default planner with streaming enabled; Luna and
Astra are additional choices.

All three use the existing `home` LiteLLM upstream with matching subscription aliases.
See [ChatGPT provider setup](docs/CHATGPT-PROVIDER.md) for the gateway-owned login
and OpenCode client configuration. The catalog's capability fields are
selection hints, not performance claims; `context_tokens: 100000` is a conservative working limit,
not a claim about full model capacity. No model fallback is configured.

![flow editor and run view](docs/screenshots/run.png)

## Quick start (kind)

Needs Docker, kind, kubectl, helm, Go, Node, and `gh` logged in.

```sh
cp .env.example .env          # LINEAR_API_KEY (optional); GITHUB_TOKEN defaults to `gh auth token`
make dev-up                   # kind cluster + images + Helm release
open http://localhost:8080
```

With the gateway configured, press **New task**, or add the trigger label to a
Linear issue. Change code
and run `make dev-reload`. `make dev-down` deletes the cluster but keeps its
state (tasks, flows, runs, transcripts) in `~/.local/share/ai-flow/ai-flow`, so
the next `make dev-up` picks up where you left off. `make dev-reset` deletes the
cluster and that state.

No cluster? `make dev-local` runs the control plane on your machine and each
step as a child process (no isolation; good for iterating).

## How it fits together

```
Linear / UI ──▶ planner ──▶ flow YAML (versioned) ──▶ engine ──▶ Job per step
                                                        ▲            │
                              gates · switches · PRs ───┘            ▼
                                                     broker: git · LLM · MCP proxies
```

- **Flows** are YAML ([format](docs/SPEC.md#4-flow-format), [examples](examples/)).
  The UI edits them as a graph, as YAML, or by chatting with the planner.
- **Node types**: `llm` (one structured call), `agent` (pi coding agent),
  `check` (shell command), `gate` (human decision), `switch` (CEL), `action`
  (open a PR, comment on the task).
- **Per-step models and limits**: each step picks a model from the catalog and
  says what happens on rate limits, quota, context overflow or budget:
  retry, wait, fall back to another model, route to another node, or fail.
- **Grants**: a step gets exactly the repo access (read, or write to its run
  branch only), MCP tools, secrets and egress it lists. Pods reach nothing but
  the control plane's pod port.
- **Config** lives in [`deploy/config`](deploy/config): the environment (where
  things run), the catalog (the menu the planner picks from) and projects (repo,
  allow-lists, budgets, start mode, planner guidance, Linear link).

Read [docs/SPEC.md](docs/SPEC.md) for the design, the security model and what
changed from the original draft.

## Templates and evaluations

Start with a reusable [flow template](docs/TEMPLATES.md): **test → implement →
verify**, **investigate → propose → approve**, or **review → repair**. Replace
the validation-only project/runtime/grant placeholders with your catalog
choices; model aliases already use Sol/Luna. Then validate against your config.
`make test-templates` validates all
three offline using the actual flow parser and resolver.

The standalone [`ai-flow-eval`](docs/EVALUATIONS.md) command scores exported
run/visit API JSON against pinned flow versions, explicit outcome/output checks
and resource limits, then compares machine-readable reports to a baseline.
Evaluation pass rate is distinct from run success status. Reports include
duration, reported cost/tokens, per-node failures and explicitly defined gate
correction counts. The included examples are **synthetic fixtures, not model
benchmark results**:

```sh
make build-eval
./bin/ai-flow-eval -suite examples/evals/baseline-suite.json > /tmp/baseline.json
./bin/ai-flow-eval -suite examples/evals/candidate-suite.json \
  -baseline /tmp/baseline.json > /tmp/candidate.json  # intentional regression; exits 1
```

CI runs Go tests/race/vet, frontend install/build and desktop/mobile browser tests, Helm
lint/render for default and kind values with both secret modes, synthetic
evaluation checks and git diff checks. No live model or production access is
needed for evaluations or template validation.

See [reliability and recovery](docs/RELIABILITY.md) for restart behavior,
execution snapshots, readiness, budget semantics and transcript limits.

## Commands

| | |
|---|---|
| `make dev-up` / `dev-reload` / `dev-down` | kind cluster lifecycle (state survives `dev-down`) |
| `make dev-reset` | delete the cluster and its state |
| `make dev-local` | control plane + demo MCP server on the host, steps as processes |
| `make test` / `make lint` | Go tests / vet + helm lint |
| `make test-race` / `make helm-check` | Go race detector / offline default+kind Helm lint and rendering |
| `make build-eval` / `make test-eval` | build standalone evaluator / evaluator and fixture tests |
| `make test-templates` / `make eval-demo` | offline template validation / synthetic passing evaluation JSON |
| `ai-flow validate -config deploy/config flow.yaml` | validate a flow |
| `ai-flow plan -config deploy/config -project sandbox -title "..."` | try the planner |
| `cd web && npm run dev` | UI with hot reload, proxied to a control plane on :8080 |
| `cd web && npx playwright install chromium && npm test` | desktop/mobile editor regressions, using mocked API responses |

## Layout

```
cmd/ai-flow        main binary: server, node (pod entrypoint), validate, plan, mcp-demo
cmd/ai-flow-eval   standalone offline export evaluator and baseline comparison
internal/          engine, broker (git/LLM/MCP proxies), runner (node-runner + limit shim),
                   planner, resolve (validation), intake (Linear), store (SQLite), ...
pi-ext/            pi extension: flow_finish + granted MCP tools
web/               React UI (embedded into the binary)
deploy/            Helm chart, kind config, images, config for kind and local mode
examples/          flows: MCP grant, token limits, sandbox red-team probe
                   templates/ (reusable flows), evals/ (synthetic evaluation fixtures)
```
