# ai-flow

ai-flow connects [Linear](https://linear.app) issues to AI-powered pipelines. When an issue transitions to a configured workflow state, ai-flow runs a command (like an AI coding agent), posts the output as a Linear comment, and moves the issue forward. It can manage the full git lifecycle — clone, branch, commit, push, and open a PR — so the subprocess only needs to write files.

With the multi-stage pipeline, you can go from a Linear ticket to a PR ready for human review with zero manual steps.

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

## Connecting Linear Projects to Git Repos

ai-flow bridges two systems: **Linear** (for project management and issue tracking) and **GitHub** (for code). Here's how they connect:

### The Relationship

- **Linear** provides the workflow: issues, states, labels, and webhooks that drive the pipeline
- **GitHub** provides the code: the repo where AI agents write code, push branches, and open PRs
- **ai-flow** is the bridge: it listens for Linear state changes and orchestrates git operations

The connection between a Linear project and a GitHub repo is defined in the **Linear project description** using YAML frontmatter. Each Linear project maps to one GitHub repo, so a single ai-flow instance can handle multiple repos — one per project.

Every issue that uses git stages (`creates_pr` or `uses_branch`) **must belong to a Linear project** with the repo metadata in its description.

### Project Description Format

Add YAML frontmatter to your Linear project's description:

```
---
github_repo: your-org/your-repo
default_branch: main
---
Rest of your project description here...
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `github_repo` | Yes | — | GitHub `owner/repo` (e.g. `acme/backend`) |
| `default_branch` | No | `main` | Base branch for new PRs |

### Configuration

```yaml
# Linear side: which team to listen to
linear:
  api_key: "${LINEAR_API_KEY}"
  webhook_secret: "${LINEAR_WEBHOOK_SECRET}"
  team_key: "ENG"              # Your Linear team key (visible in team settings)
```

The `team_key` is found in Linear under **Settings > Teams > [Your Team]** — it's the short prefix like `ENG`, `PROD`, etc. that appears before issue numbers (e.g. `ENG-123`).

The `github_repo` in each project description uses the `owner/repo` format used by GitHub (e.g. `acme/backend`). ai-flow clones via HTTPS using `gh` authentication.

### What You Need Set Up Before Running

1. **Linear API key** — Create at **Linear Settings > API > Personal API keys** (or use an OAuth app)
2. **Linear webhook** — Create at **Linear Settings > API > Webhooks**, pointing to your ai-flow URL
3. **GitHub CLI (`gh`)** — Install and authenticate with `gh auth login`
4. **Git** — Must be installed and on PATH
5. **Linear workflow states** — Must match the `linear_state` and `next_state` values in your pipeline
6. **Linear projects** — Each project that uses git stages must have YAML frontmatter with `github_repo` in its description

## Setting Up Git / PR Creation

For ai-flow to create PRs and commit code, you need three things:

### 1. Install `git` and `gh`

ai-flow uses the `git` CLI for cloning/branching/pushing and the [GitHub CLI (`gh`)](https://cli.github.com/) for creating pull requests. Both must be installed and on your PATH.

```sh
# Authenticate gh (one-time)
gh auth login
```

ai-flow automatically configures git identity (`user.name` and `user.email`) in each temp clone, so you don't need global git config on the server.

### 2. Add repo metadata to your Linear project

Add YAML frontmatter to your Linear project's description:

```
---
github_repo: your-org/your-repo
default_branch: main
---
```

Every issue that triggers a git stage must belong to a Linear project with this metadata.

### 3. Use `creates_pr` or `uses_branch` on pipeline stages

- **`creates_pr: true`** — ai-flow clones the repo, creates a new feature branch (named from the issue identifier + title, e.g. `eng-123-add-auth`), runs your command inside the clone, then commits all changes, pushes, and opens a PR. Use this on the **first** stage that writes code (e.g. "implement").

- **`uses_branch: true`** — ai-flow looks up the existing branch from the first `creates_pr` stage's run, clones the repo, checks out that branch, runs your command, and pushes any new commits. The PR updates automatically, and a comment is posted on the PR noting the stage that pushed. Use this for downstream stages that review or modify existing code (e.g. "security", "test", "review").

If neither is set, the stage runs without git — it just executes the command and posts the output as a comment. This is useful for planning or triage stages that only produce analysis.

## The Autonomous Flow (End to End)

Here's exactly what happens when you move an issue through a full pipeline:

### 1. You create an issue in Linear and add the "auto" label

The issue starts in Backlog or wherever your team's default is.

### 2. You (or automation) move the issue to "Todo"

This triggers the **plan** stage:
- ai-flow receives a webhook from Linear
- Runs your planning command with the issue context
- Posts the plan as a comment on the Linear issue
- Moves the issue to "In Progress"

### 3. The "In Progress" state triggers the **implement** stage

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

**Your only manual steps are:**
1. Create the issue and add the label
2. Move it to "Todo" to start the pipeline
3. Review and merge the final PR

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

This is the recommended setup for fully autonomous ticket-to-PR. Issues must belong to a Linear project with `github_repo` in its description frontmatter.

```yaml
server:
  port: 8080

linear:
  api_key: "${LINEAR_API_KEY}"
  webhook_secret: "${LINEAR_WEBHOOK_SECRET}"
  team_key: "ENG"

pipeline:
  # 1. Plan: analyze the issue, break it down (no git needed)
  - name: "plan"
    linear_state: "Todo"
    command: "claude-code"
    args: ["--print"]
    prompt: |
      Analyze this issue and create a detailed implementation plan.
      Break down the work into clear steps and identify relevant files.
    next_state: "In Progress"
    timeout: 300
    labels: ["auto"]

  # 2. Implement: write code, create PR (first git stage)
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
    creates_pr: true

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
Todo → In Progress → Security Review → Testing → Review → Done
(plan)  (implement)    (security)       (test)    (review)  (human reviews PR)
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

## Linear Setup

### Workflow States

Your Linear team needs workflow states that match the `linear_state` and `next_state` values in your pipeline. For the full 5-stage pipeline, you need:

- **Todo** (type: backlog or unstarted)
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
| `prompt` | — | Prompt template prepended with issue context |
| `next_state` | — | Linear state to transition to on exit 0 |
| `failure_state` | — | Linear state to transition to on failure (exit 1) |
| `timeout` | `300` | Subprocess timeout in seconds |
| `labels` | `[]` | Only run for issues with at least one of these labels (empty = all) |
| `creates_pr` | `false` | Clone repo, create branch, commit, push, open PR |
| `uses_branch` | `false` | Checkout existing branch from a prior `creates_pr` stage |
| `wait_for_approval` | `false` | Don't auto-transition; post output and wait for a comment to re-run |

**Constraints:**
- `creates_pr` and `uses_branch` are mutually exclusive
- Both require the issue to belong to a Linear project with `github_repo` in its description frontmatter
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

Every subprocess receives these environment variables (when `context_mode` is `env` or `both`):

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
