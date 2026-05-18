# ai-flow

ai-flow connects [Linear](https://linear.app) issues to AI-powered pipelines. When an issue transitions to a configured workflow state, ai-flow runs a command (like an AI coding agent), posts the output as a Linear comment, and moves the issue forward. It can manage the full git lifecycle — clone, branch, commit, push, and open a PR — so the subprocess only needs to write files.

With the multi-stage pipeline, you can go from a Linear ticket to a PR ready for human review while keeping normal human interaction inside Linear and GitHub.

## Product Vision

ai-flow is built around a simple operating model: humans stay in planning and review tools, while AI workers handle the command line, codebase navigation, implementation, verification, and PR updates.

The human should not need to sit near the terminal or shepherd an agent session. The human interface should be Linear and the pull request:

- **Linear** is where intent, prioritization, decomposition, approval, and feedback happen.
- **GitHub PRs** are where the final code, tests, docs, planning artifacts, and review evidence are inspected.
- **ai-flow** is the automation layer that turns Linear workflow movement and comments into repeatable AI worker runs.
- **AI workers** do the hands-on coding work and leave behind artifacts that make the result reviewable.

The goal is not just to produce code automatically. The goal is to produce changes that humans are confident enough to accept and merge.

### Success Metric

The core success metric for ai-flow is accepted PRs after the automated flow.

Good automation should increase:

- PRs accepted with minimal human rework
- small, focused PRs instead of broad risky diffs
- clear test evidence and review artifacts
- reviewer confidence that the code matches the Linear request

Bad automation is not just a failed run. A PR that reaches review but is too large, under-tested, poorly explained, or misaligned with the ticket is also a failure of the flow.

### Confidence Model

ai-flow should build reviewer confidence by producing evidence alongside code:

- small Linear issues with narrow scope
- project-to-issue decomposition for larger work
- OpenSpec proposals, specs, designs, and task lists for ambiguous work
- tests that map back to expected behavior
- review, security, and test stage comments
- PR comments showing which automated stages modified the branch
- docs updates when behavior or usage changes

The preferred improvement loop is: make the issue smaller, make the expected behavior clearer, make the verification stronger, then let the worker proceed.

## Quick Start

```sh
# Build
go build ./cmd/ai-flow

# Configure
cp config.example.yaml config.yaml
# Edit config.yaml with your settings

# Run
export LINEAR_API_KEY="lin_api_..."
export LINEAR_WEBHOOK_SECRET="..."
./ai-flow -config config.yaml -db ai-flow.db
```

## How It Works

```
Linear webhook → ai-flow → match pipeline stage → run subprocess → post comment + transition
                                                 ↘ (if git stage) clone → branch → commit → push → PR
```

1. An issue moves to a workflow state that matches a pipeline stage (e.g. "Todo")
2. ai-flow checks that the issue has the required labels (if configured)
3. The stage's command runs with full issue context (env vars, CLI args, and/or stdin)
4. Based on the exit code:
   - **0** — success: post output as a comment, transition to `next_state`
   - **1** — failure: post error as a comment, transition to `failure_state` (if configured)
   - **2** — skip: do nothing (no comment, no transition)

## Connecting Issues to Git Repos

ai-flow bridges two systems: **Linear** (for project management and issue tracking) and **GitHub** (for code). Here's how they connect:

### The Relationship

- **Linear** provides the workflow: issues, states, labels, and webhooks that drive the pipeline
- **GitHub** provides the code: the repo where AI agents write code, push branches, and open PRs
- **ai-flow** is the bridge: it listens for Linear state changes and orchestrates git operations

Repo selection is defined per issue, with an optional flow-level GitHub owner in `config.yaml`. That lets a ticket say just the repo name in frontmatter, or even mention the repo naturally in the title/description when the owner is fixed for the flow.

Every issue that uses git stages (`creates_pr` or `uses_branch`) must identify a repo explicitly or be specific enough for ai-flow to infer one from the issue text.

### Issue Description Format

Add YAML frontmatter to your Linear issue description when you want to be explicit:

```
---
github_repo: your-org/your-repo
default_branch: main
---
Rest of your issue description here...
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `github_repo` | No* | — | GitHub repo as `owner/repo`, or just `repo` when `github.owner` is set |
| `default_branch` | No | `main` | Base branch for new PRs |

`*` If `github_repo` is omitted, ai-flow will try to infer the repo from the issue title and description using the repos visible under `github.owner`.

### Configuration

```yaml
github:
  owner: "acme"               # Optional default org/user for repo resolution

# Linear side: which team to listen to
linear:
  api_key: "${LINEAR_API_KEY}"
  webhook_secret: "${LINEAR_WEBHOOK_SECRET}"
  team_key: "ENG"              # Your Linear team key (visible in team settings)
```

The `team_key` is found in Linear under **Settings > Teams > [Your Team]** — it's the short prefix like `ENG`, `PROD`, etc. that appears before issue numbers (e.g. `ENG-123`).

When `github.owner` is set, `github_repo` may be either `owner/repo` or just `repo`. If you omit `github_repo`, ai-flow tries to match a repo from natural language in the issue text. ai-flow clones via HTTPS using `gh` authentication.

### What You Need Set Up Before Running

1. **Linear API key** — Create at **Linear Settings > API > Personal API keys** (or use an OAuth app)
2. **Linear webhook** — Create at **Linear Settings > API > Webhooks**, pointing to your ai-flow URL
3. **GitHub CLI (`gh`)** — Install and authenticate with `gh auth login`
4. **Git** — Must be installed and on PATH
5. **Linear workflow states** — Must match the `linear_state` and `next_state` values in your pipeline
6. **GitHub access** — `gh` must be able to list and clone the repos ai-flow may use

## Setting Up Git / PR Creation

For ai-flow to create PRs and commit code, you need three things:

### 1. Install `git` and `gh`

ai-flow uses the `git` CLI for cloning/branching/pushing and the [GitHub CLI (`gh`)](https://cli.github.com/) for creating pull requests. Both must be installed and on your PATH.

```sh
# Authenticate gh (one-time)
gh auth login
```

ai-flow automatically configures git identity (`user.name` and `user.email`) in each temp clone, so you don't need global git config on the server.

### 2. Add repo metadata to your Linear issue (or set a default owner)

Set a default owner in `config.yaml` if most tickets target the same GitHub org/user:

```yaml
github:
  owner: "your-org"
```

Then optionally add YAML frontmatter to an issue description:

```
---
github_repo: your-repo
default_branch: main
---
```

Or skip frontmatter and mention the repo naturally in the issue title/description; ai-flow will try to infer it from repos under `github.owner`.

### 3. Use `creates_pr` or `uses_branch` on pipeline stages

- **`creates_pr: true`** — ai-flow clones the repo, creates a new feature branch (named from the issue identifier + title, e.g. `eng-123-add-auth`), runs your command inside the clone, then commits all changes, pushes, and opens a PR. Use this on the **first** stage that writes code (e.g. "implement").

- **`uses_branch: true`** — ai-flow looks up the existing branch from the first `creates_pr` stage's run, clones the repo, checks out that branch, runs your command, and pushes any new commits. The PR updates automatically, and a comment is posted on the PR noting the stage that pushed. Use this for downstream stages that review or modify existing code (e.g. "security", "test", "review").

If neither is set, the stage runs without git — it just executes the command and posts the output as a comment. This is useful for planning or triage stages that only produce analysis.

## Human Operating Model

Humans should operate ai-flow from Linear and GitHub, not from the terminal.

For day-to-day use, the human actions are:

1. Create or refine a Linear issue or project.
2. Add the configured automation label, such as `auto`.
3. Move the issue or project into the configured trigger state.
4. Review planning artifacts, questions, and status updates in Linear or the PR.
5. Give feedback through Linear comments or PR review comments.
6. Approve by moving the issue to the next Linear state or by accepting the PR.

The terminal setup is an operator concern for running ai-flow itself. It should not be part of the normal product/engineering review loop once the service is running.

## The Autonomous Flow (End to End)

Here's exactly what happens when you move an issue through a full pipeline:

### 1. You create an issue in Linear and add the "auto" label

The issue starts in Backlog or wherever your team's default is.

### 2. You (or automation) move the issue to "Todo"

This triggers the **plan** stage:
- ai-flow receives a webhook from Linear
- Runs your planning command with the issue context
- Posts the plan as a comment on the Linear issue, or opens a planning PR when using OpenSpec
- Either moves the issue forward automatically or waits for human approval in Linear/PR review

### 3. The human-approved "In Progress" state triggers the **implement** stage

- ai-flow creates a temp directory (sandbox)
- Clones the repo into it (`git clone --depth 1`)
- Creates a feature branch: `eng-123-add-user-auth`
- Configures git identity in the clone
- Runs your command inside the clone directory
- If the command succeeds (exit 0):
  - Commits all changes: `ENG-123: Add user auth`
  - Pushes the branch
  - Opens a PR via `gh pr create`
  - Posts the PR link as a comment on the Linear issue
  - Moves the issue to "Security Review"
- Cleans up the temp directory

### 4. Downstream stages (security, test, review) run on the same branch

Each `uses_branch` stage:
- Looks up the branch from the implement stage's run
- Clones, checks out the existing branch
- Runs the command in the clone
- Commits and pushes any changes (the PR updates automatically)
- Posts a comment on both the Linear issue and the GitHub PR
- Moves the issue forward

### 5. If a stage fails, the issue cycles back

When security, test, or review fails (exit 1):
- ai-flow posts the error as a comment on the Linear issue
- Moves the issue back to the `failure_state` (e.g. "In Progress")
- This re-triggers the implement stage, which now:
  - Sees the existing branch on remote
  - Checks it out instead of creating a new one
  - Reads all previous comments (including the failure feedback) for context
  - Pushes fixes to the same branch, updating the same PR

### 6. When all stages pass, the issue reaches "Done"

The PR is ready for human review. You review the PR on GitHub, merge it, and close the issue.

**The normal human steps are:**
1. Create or refine the Linear issue and add the automation label
2. Move it to the trigger state, such as "Todo"
3. Review plan evidence in Linear or the planning PR when a planning gate is used
4. Review and merge the final PR

## OpenSpec Planning Gate

ai-flow can use [OpenSpec](https://openspec.dev/) as a human-reviewed planning layer before implementation. In this mode, the first stage creates durable OpenSpec artifacts in the target repo instead of only posting a transient plan comment.

This is the preferred flow when human confidence depends on understanding intent before reading code. The human still does not need to run OpenSpec or inspect the local checkout. They review the generated artifacts in the PR and give feedback in Linear or GitHub.

The planning PR contains:

- `openspec/changes/<change>/proposal.md` for intent, scope, and non-goals
- `openspec/changes/<change>/specs/` for behavior deltas and acceptance scenarios
- `openspec/changes/<change>/design.md` for technical approach and tradeoffs
- `openspec/changes/<change>/tasks.md` for implementation steps

### Target Repo Setup

Each repo that uses the OpenSpec planning stage must have OpenSpec initialized before ai-flow runs against it:

```sh
npm install -g @fission-ai/openspec@latest
openspec init
```

Commit the generated OpenSpec project files according to that repo's policy. ai-flow does not initialize OpenSpec automatically; it expects the target repo to already contain an `openspec/` directory.

### Human-in-the-loop Flow

Use `prompts/openspec-plan.md` with a git-enabled planning stage:

```yaml
pipeline:
  - name: "openspec-plan"
    linear_state: "Todo"
    command: "claude"
    args: ["-p", "--model", "sonnet", "--dangerously-skip-permissions"]
    prompt_file: "prompts/openspec-plan.md"
    next_state: "In Progress"
    timeout: 7200
    labels: ["auto"]
    creates_pr: true
    wait_for_approval: true

  - name: "implement"
    linear_state: "In Progress"
    command: "opencode"
    args: ["run"]
    prompt_file: "prompts/implement.md"
    next_state: "Testing"
    timeout: 7200
    labels: ["auto"]
    uses_branch: true
```

With `wait_for_approval: true`, ai-flow posts the planning output and PR link but does not transition the issue automatically. Review the OpenSpec artifacts in the PR, leave Linear comments for requested changes, and rerun the planning stage through comment-triggered approval feedback. When the plan is acceptable, manually move the Linear issue to `In Progress`; the implementation stage reuses the same branch and treats the OpenSpec artifacts as the approved scope.

Webhook mode is recommended for this flow because Linear comment webhooks can trigger planning re-runs. Poll mode can still use the manual state gate, but comment-triggered re-runs are limited.

### When to Use OpenSpec

Use OpenSpec planning for:

- ambiguous product or architecture work
- API, data model, or contract changes
- security-sensitive behavior
- multi-step features
- changes where reviewers need to understand intent before code

Use a simpler no-git planning or direct implementation stage for obvious small fixes, mechanical cleanup, and low-risk chores.

### Planning for Small PRs

ai-flow works best when each issue can become a small, reviewable PR. For larger requests, use a planning or project stage to split work before implementation starts.

Good issue decomposition should produce tickets that:

- change one behavior or one bounded area
- have clear acceptance criteria
- can be tested independently
- can be reviewed in one focused PR
- avoid mixing feature work with unrelated cleanup

If a planning agent discovers that an issue is too broad, it should say so and propose smaller Linear issues instead of pushing ahead with a large implementation.

### Review Evidence

Every automated PR should help the reviewer answer four questions:

- **What was requested?** Link back to the Linear issue and, when used, OpenSpec proposal/specs.
- **What changed?** Keep the diff focused and explain important design choices.
- **How was it verified?** Include test results, added coverage, and any known gaps.
- **Why should this be trusted?** Include review/security/test stage outputs and unresolved risks.

The pipeline should optimize for these review signals because they are what turn an automated branch into an accepted PR.

## Writing a Good Config

### Minimal: Single Stage

The simplest config runs one command when an issue hits a specific state:

```yaml
server:
  port: 8080

linear:
  api_key: "${LINEAR_API_KEY}"
  webhook_secret: "${LINEAR_WEBHOOK_SECRET}"
  team_key: "ENG"

pipeline:
  - name: "triage"
    linear_state: "Triage"
    command: "my-script"
    args: ["--analyze"]
    prompt: |
      Analyze this issue and provide a summary.
    next_state: "Todo"
    timeout: 120

subprocess:
  context_mode: "env"
  max_concurrent: 3
```

No Linear project metadata needed since no stage creates PRs.

### Full Pipeline: Plan through Review

This is the recommended setup for fully autonomous ticket-to-PR. Each issue must either provide `github_repo` in its description or be resolvable from `github.owner` plus the issue text.

```yaml
server:
  port: 8080

linear:
  api_key: "${LINEAR_API_KEY}"
  webhook_secret: "${LINEAR_WEBHOOK_SECRET}"
  team_key: "ENG"

pipeline:
  # 1. OpenSpec plan: create reviewable planning artifacts in the repo
  - name: "openspec-plan"
    linear_state: "Todo"
    command: "claude-code"
    args: ["--print"]
    prompt_file: "prompts/openspec-plan.md"
    next_state: "In Progress"
    timeout: 300
    labels: ["auto"]
    creates_pr: true
    wait_for_approval: true

  # 2. Implement: write code on the approved planning branch
  - name: "implement"
    linear_state: "In Progress"
    command: "claude-code"
    args: ["--print"]
    prompt: |
      Implement the changes for this issue. Follow the project's
      coding conventions and include tests.
    next_state: "Security Review"
    timeout: 600
    labels: ["auto"]
    uses_branch: true

  # 3. Security review on existing branch
  - name: "security"
    linear_state: "Security Review"
    command: "claude-code"
    args: ["--print"]
    prompt: |
      Review the code on this branch for security vulnerabilities.
      Fix any issues directly. Exit 0 if safe, exit 1 if not.
    next_state: "Testing"
    failure_state: "In Progress"
    timeout: 600
    labels: ["auto"]
    uses_branch: true

  # 4. Test on existing branch
  - name: "test"
    linear_state: "Testing"
    command: "claude-code"
    args: ["--print"]
    prompt: |
      Run the test suite and add missing coverage.
      Fix failing tests directly. Exit 0 if passing, exit 1 if not.
    next_state: "Review"
    failure_state: "In Progress"
    timeout: 600
    labels: ["auto"]
    uses_branch: true

  # 5. Code review on existing branch
  - name: "review"
    linear_state: "Review"
    command: "claude-code"
    args: ["--print"]
    prompt: |
      Review code quality and correctness. Fix issues directly.
      Exit 0 if ready for human review, exit 1 if not.
    next_state: "Done"
    failure_state: "In Progress"
    timeout: 600
    labels: ["auto"]
    uses_branch: true

subprocess:
  context_mode: "env"
  max_concurrent: 3
```

### Pipeline Flow

```
Todo ──human approves plan──> In Progress → Security Review → Testing → Review → Done
(OpenSpec plan PR)            (implement)    (security)       (test)    (review)  (human reviews PR)
                                             ↓               ↓         ↓
                                          In Progress     In Progress  In Progress
                                          (on failure)    (on failure) (on failure)
```

When a stage with `failure_state` fails (exit code 1), the issue moves back to that state, which re-triggers the earlier stage. For example: security finds a vulnerability → issue goes back to "In Progress" → the implement stage re-runs with the security feedback as context (from Linear comments) → pushes to the same branch → issue moves to "Security Review" again.

### Tips for Good Prompts

- Be specific about what the stage should do and what exit codes mean
- The subprocess receives **all Linear comments** as context, including ai-flow's own stage output comments. This means downstream stages can see what upstream stages did and any failure feedback
- For `uses_branch` stages, tell the agent it's working on an existing branch with existing changes
- The composed prompt includes the issue identifier, title, description, URL, and labels automatically — you don't need to repeat that in your prompt
- Ask agents to keep PRs small, call out when a ticket should be split, and include verification evidence in their stage output
- Tell review and test agents to compare the implementation against Linear acceptance criteria and OpenSpec artifacts when present

## Linear Setup

### Workflow States

Your Linear team needs workflow states that match the `linear_state` and `next_state` values in your pipeline. For the full 5-stage pipeline, you need:

- **Todo** (type: backlog or unstarted)
- **Plan Review** (type: started) — optional, useful when planning approval is separate from implementation
- **In Progress** (type: started)
- **Security Review** (type: started) — create this
- **Testing** (type: started) — create this
- **Review** (type: started) — create this
- **Done** (type: completed)

Create custom states in **Linear Settings > Teams > [Your Team] > Workflow**.

ai-flow validates all pipeline states against Linear on startup. If a state doesn't exist, it will exit with an error telling you which state is missing.

### Webhook

1. Go to **Linear Settings > API > Webhooks**
2. Create a webhook pointing to `https://your-host:8080/webhook`
3. Select **Issue** and **Comment** events (comments are needed for `wait_for_approval` re-runs)
4. Copy the signing secret into your config as `LINEAR_WEBHOOK_SECRET`

The webhook must be reachable from Linear's servers. For local development, use a tunnel like `ngrok` or `cloudflared`.

### Labels

Use the `labels` field to control which issues trigger a stage. For example, `labels: ["auto"]` means only issues with the "auto" label will be processed. Create the label in Linear and add it to issues you want ai-flow to handle.

If `labels` is empty or omitted, the stage matches **all** issues in that state.

## Reliability & Recovery

### Crash Recovery

ai-flow tracks all runs in a SQLite database. On startup, it automatically recovers any "running" records older than 10 minutes — these are zombie records from a previous crash. They're marked as failed so the pipeline can retry.

### Deduplication

If the same issue+stage combination is already running, ai-flow skips the duplicate webhook. This prevents parallel execution of the same work.

### Work Queue

Webhook and poller discoveries are placed onto an internal bounded work queue before orchestration starts. This avoids spawning unbounded processing goroutines when many Linear issues or projects match at once, and gives ai-flow one shared concurrency control point for issue and project work.

### Retry on API Failures

Linear API calls use exponential backoff with up to 3 retries. Transient network issues won't kill a pipeline run.

### Output Limits

Subprocess stdout and stderr are capped at 1 MB each to prevent memory issues from runaway processes. Output beyond the limit is truncated with a note.

### Sandbox Isolation

Each git stage runs in a fresh temp directory that is cleaned up after the stage completes. Stages never share a working directory — each gets its own clone.

## Configuration Reference

### `server`

| Field | Default | Description |
|-------|---------|-------------|
| `port` | `8080` | HTTP server port |

### `linear`

| Field | Required | Description |
|-------|----------|-------------|
| `api_key` | Yes | Linear API key (create at Settings > API > Personal API keys) |
| `webhook_secret` | Yes | Webhook signing secret (from Settings > API > Webhooks) |
| `team_key` | Yes | Linear team key — the prefix before issue numbers (e.g. `ENG` for `ENG-123`) |

### `pipeline[]`

| Field | Default | Description |
|-------|---------|-------------|
| `name` | — | Stage identifier (must be unique) |
| `linear_state` | — | Trigger when issue enters this state |
| `command` | — | Command to execute |
| `args` | `[]` | Command arguments (composed prompt appended as final arg) |
| `prompt_file` | — | Prompt file prepended with issue context |
| `next_state` | — | Linear state to transition to on exit 0 |
| `failure_state` | — | Linear state to transition to on failure (exit 1) |
| `timeout` | `3600` | Subprocess timeout in seconds |
| `labels` | `[]` | Only run for issues with at least one of these labels (empty = all) |
| `creates_pr` | `false` | Clone repo, create branch, commit, push, open PR |
| `uses_branch` | `false` | Checkout existing branch from a prior `creates_pr` stage |
| `wait_for_approval` | `false` | Don't auto-transition; post output and wait for human action or a comment-triggered re-run |

**Constraints:**
- `creates_pr` and `uses_branch` are mutually exclusive
- Both require ai-flow to resolve a GitHub repo for the issue
- `failure_state` cannot be the same as `linear_state`
- Each `linear_state` must be unique across the pipeline
- Only **one** stage should have `creates_pr: true` per pipeline — downstream stages use `uses_branch: true`

### `subprocess`

| Field | Default | Description |
|-------|---------|-------------|
| `context_mode` | `env` | How to pass context: `env`, `stdin`, or `both` |
| `max_concurrent` | `3` | Max parallel subprocess runs |

## Subprocess Interface

### Exit Codes

| Code | Meaning | Behavior |
|------|---------|----------|
| `0` | Success | Transition to `next_state`, post output as comment |
| `1` | Failure | Transition to `failure_state` (if set), post error as comment |
| `2` | Skip | No transition, no comment |

### Environment Variables

Every subprocess receives these environment variables:

| Variable | Description |
|----------|-------------|
| `AIFLOW_ISSUE_ID` | Linear issue ID |
| `AIFLOW_ISSUE_IDENTIFIER` | Issue identifier (e.g. `ENG-123`) |
| `AIFLOW_ISSUE_TITLE` | Issue title |
| `AIFLOW_ISSUE_DESCRIPTION` | Issue description |
| `AIFLOW_ISSUE_URL` | Linear issue URL |
| `AIFLOW_ISSUE_STATE` | Current workflow state name |
| `AIFLOW_ISSUE_LABELS` | Comma-separated label names |
| `AIFLOW_STAGE_NAME` | Pipeline stage name |
| `AIFLOW_NEXT_STATE` | Target state on success |
| `AIFLOW_PROMPT` | Composed prompt (issue context + stage prompt + comments) |
| `AIFLOW_WORK_DIR` | Clone directory (only for git stages) |
| `AIFLOW_BRANCH` | Git branch name (only for git stages) |
| `AIFLOW_COMMENTS` | JSON array of comments (when comments exist) |

### Stdin (JSON)

When `context_mode` is `stdin` or `both`, a JSON object is piped to stdin with all the issue context, stage config, and comments.

### CLI Args

The composed prompt (issue context + your prompt template + comments) is appended as the final CLI argument after your configured `args`.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/webhook` | Linear webhook receiver (HMAC-SHA256 verified) |
| `GET` | `/health` | Health check (`{"status":"ok"}`) |

## Architecture

```
cmd/ai-flow/          Entry point, startup validation, crash recovery
internal/
  config/              YAML config loading and validation
  linear/              Linear API client (with retry) and webhook handler
  git/                 Git/GitHub CLI wrapper (clone, branch, commit, push, PR, PR comments)
  subprocess/          Command execution with concurrency control and output limits
  orchestrator/        Pipeline coordination (webhook → subprocess → Linear + GitHub)
  store/               SQLite persistence for run dedup, branch tracking, crash recovery
```

## License

MIT
