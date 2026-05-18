# OpenSpec Integration Plan

## Goal

Add OpenSpec as the durable planning layer for ai-flow so humans review intent, scope, requirements, design, and tasks before autonomous implementation begins.

Target workflow:

```text
Linear issue/project
-> ai-flow planning stage creates OpenSpec artifacts
-> human reviews and refines plan
-> human releases implementation
-> agents implement/test/review against OpenSpec artifacts
-> final human PR review
```

## Principles

- Keep implementation autonomous after approval.
- Put human input at planning boundaries, not every coding step.
- Use existing ai-flow primitives first: Linear states, git stages, comments, `wait_for_approval`, branch reuse.
- Treat OpenSpec artifacts as repo-owned source files, not transient Linear comments.
- Start repo-local; avoid depending on OpenSpec workspace/multi-repo beta behavior.

## Phase 1: MVP

### 1. Define the Linear Workflow

Recommended states:

```text
Todo -> Plan Review -> In Progress -> Testing -> Security Review -> Overall Review -> Done
```

Behavior:

- `Todo`: planning stage runs.
- `Plan Review`: human reviews OpenSpec artifacts and comments requested changes.
- `In Progress`: implementation begins only after human manually moves the issue forward.

### 2. Add OpenSpec Planning Prompt

Create `prompts/openspec-plan.md`.

Prompt requirements:

- Verify OpenSpec is initialized in the target repo or explain that setup is missing.
- Create one OpenSpec change folder for the Linear issue.
- Generate planning artifacts: `proposal.md`, delta specs, `design.md`, and `tasks.md`.
- Keep specs behavior-focused and implementation details in `design.md`/`tasks.md`.
- Include unresolved questions clearly if requirements are ambiguous.
- Do not write product code during planning.
- Exit `0` when artifacts are ready for human review.
- Exit `1` only for real failures.

### 3. Update Example Config

Add an OpenSpec-oriented pipeline example.

Planning stage:

```yaml
- name: "openspec-plan"
  linear_state: "Todo"
  command: "opencode"
  args: ["run"]
  prompt_file: "prompts/openspec-plan.md"
  next_state: "Plan Review"
  timeout: 7200
  labels: ["auto"]
  creates_pr: true
  wait_for_approval: true
```

Implementation stage:

```yaml
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

Important MVP behavior:

- `wait_for_approval: true` keeps the issue in `Plan Review` after posting output.
- Human comments can trigger planning re-runs/refinements.
- Human manually moves issue to `In Progress` when satisfied.

### 4. Update Implementation Prompt

Revise `prompts/implement.md` so the implementation agent:

- Finds the active OpenSpec change for the issue.
- Reads `proposal.md`, `design.md`, `tasks.md`, and delta specs before editing code.
- Implements only the approved scope.
- Updates `tasks.md` checkboxes as work completes.
- Updates OpenSpec artifacts if implementation discovers a necessary plan correction.
- Does not ignore unresolved questions.

### 5. Update Review/Test Prompts

Revise review prompts so downstream agents verify code against OpenSpec artifacts:

- `prompts/test.md`: map tests to OpenSpec scenarios where practical.
- `prompts/review.md`: review implementation against proposal, specs, design, and tasks.
- `prompts/security.md`: check whether security-sensitive requirements are reflected in specs/design.

### 6. Document Setup

Update README with an OpenSpec planning section:

- Install: `npm install -g @fission-ai/openspec@latest`.
- Initialize target repos: `openspec init`.
- Commit generated OpenSpec instructions/skills according to each repo's policy.
- Explain the Linear states and manual approval movement.
- Explain when to use OpenSpec vs the simpler no-git planning stage.

### 7. Validate MVP Manually

Test with a small issue:

- Move issue to `Todo`.
- Confirm ai-flow creates branch/PR with only OpenSpec artifacts.
- Comment requested plan changes in Linear.
- Confirm comment re-runs planning stage.
- Move issue to `In Progress`.
- Confirm implementation uses same branch.
- Confirm final PR contains both OpenSpec artifacts and code.

## Phase 2: Better Approval Semantics

### 1. Add Explicit Approval Commands

Support structured Linear comments on `wait_for_approval` stages:

```text
/approve
/changes-requested <feedback>
/cancel
```

Behavior:

- `/approve`: transition to `next_state` without re-running the stage.
- `/changes-requested`: re-run current stage with the feedback included.
- `/cancel`: mark run failed or skip based on configured behavior.

### 2. Track Awaiting Approval Runs

Persist approval state in SQLite instead of treating successful wait stages as fully complete.

Suggested run statuses:

- `running`
- `awaiting_approval`
- `completed`
- `failed`
- `skipped`

### 3. Improve Comments

For wait stages, post a clearer comment:

```text
ai-flow: stage `openspec-plan` awaiting approval

PR: <url>
Review the OpenSpec artifacts, then comment `/approve` or `/changes-requested ...`.
```

### 4. Add Config Options

Add optional fields only if needed:

```yaml
approval_commands: true
approval_state: "Plan Review"
```

Keep defaults backward-compatible.

## Phase 3: OpenSpec-Aware Pipeline Features

### 1. Active Change Detection

Add helper behavior or prompt conventions to identify the relevant OpenSpec change by:

- Linear identifier in change folder name.
- Branch name.
- Metadata file in `openspec/changes/<change>/.openspec.yaml`.

### 2. Add Optional Spec Verify Stage

New stage before final review:

```text
Spec Verify -> Overall Review
```

The stage checks:

- all tasks are complete or intentionally deferred
- implementation matches delta specs
- design still reflects the actual implementation
- unresolved questions are closed

### 3. Archive Flow

Decide when OpenSpec archive/sync should happen:

- Option A: before final PR review, so specs are updated in the same PR.
- Option B: after merge, as a follow-up automation.
- Option C: leave archive manual for early versions.

MVP recommendation: leave archive manual until the team trusts the flow.

### 4. Dashboard Visibility

Expose planning state in the dashboard:

- stage waiting for approval
- PR URL
- active OpenSpec change name
- latest unresolved questions
- approval command hint

## Phase 4: Project-Level Planning

Use OpenSpec before Linear issue creation for larger projects.

Possible flow:

```text
Linear project with planning label
-> project planner creates OpenSpec project proposal
-> human reviews decomposition
-> approved planner creates Linear issues
-> each issue runs issue-level OpenSpec plan
```

MVP alternative:

- Keep current project planner JSON output.
- Add prompt guidance to produce issue descriptions that mention expected OpenSpec domains and acceptance criteria.

## Files Likely To Change

- `prompts/openspec-plan.md`: new planning prompt.
- `prompts/implement.md`: read and follow OpenSpec artifacts.
- `prompts/test.md`: verify scenarios where practical.
- `prompts/review.md`: compare code against OpenSpec plan.
- `prompts/security.md`: compare security-sensitive code against specs/design.
- `config.example.yaml`: add OpenSpec pipeline example.
- `README.md`: document OpenSpec setup and workflow.
- `internal/orchestrator/orchestrator.go`: later explicit approval commands.
- `internal/store/store.go`: later approval status persistence.
- `internal/orchestrator/orchestrator_test.go`: approval and wait-stage tests.

## Risks

- OpenSpec slash commands may not work headlessly in every subprocess agent.
- Generated OpenSpec instructions may differ by target repo and agent tool.
- Specs can drift unless implementation/review stages enforce updates.
- Full OpenSpec artifacts may be too heavy for tiny fixes.
- Current `wait_for_approval` is a pause/re-run mechanism, not a true approval gate.

## MVP Acceptance Criteria

- A Linear issue can trigger an OpenSpec planning PR.
- Planning PR contains OpenSpec artifacts and no product code.
- Human can request changes through Linear comments and re-run planning.
- Human can manually release implementation by moving the issue to `In Progress`.
- Implementation runs on the same branch and follows OpenSpec artifacts.
- Downstream stages receive OpenSpec artifacts as repo context.
- README explains setup and expected human actions.

## Unresolved Questions

- Should planning use `opencode`, `claude`, or be configurable per repo?
- Should OpenSpec initialization be ai-flow's responsibility or a prerequisite in target repos?
- Should the planning PR later become the implementation PR, or should implementation branch from the approved planning branch?
- Should `/approve` be implemented now, or is manual Linear state movement enough for MVP?
- Should completed OpenSpec changes be archived before merge, after merge, or manually?
- How lightweight should the OpenSpec schema be for small bug fixes?
