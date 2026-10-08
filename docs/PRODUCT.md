# Product work: repositories, docs and user story maps

ai-flow can start from the product instead of from a task. You link a GitHub
repository, describe the product in `product/`, lay out a user story map, and
send parts of the map to flows that build, test and ship them.

## Link a repository

**Products → Link a repository** lists the repositories the GitHub token can
see. Linking one adds two config entries, both editable later in **Settings**:

- a git grant `repo/<name>` with read and write access, and
- a project `<name>` with that repo, its default branch as the base, manual
  start, and every catalog model allowed.

Tokens are not entered in the UI. GitHub and git host tokens come from the
environment (sealed secrets in the cluster); **Settings → Connections** shows
which are set.

## The scratch checkout

Each project has a scratch copy of its `product/` directory on the control
plane's disk (`<dataDir>/workspaces/<project>/`). Everything you change in the UI, and
every change you apply from the map assistant, is written there first and
nothing else happens until you choose to commit.

- **Commit & push** makes one commit with every local change directly on the
  project's base branch. It never force-pushes: if someone pushed since your
  last pull, it is refused and you pull first.
- **Pull** brings in newer commits. It keeps your local edits, and refuses
  (listing the files) when the remote changed a file you have also edited;
  discard your edits to those files, then pull again.
- **Discard** drops local edits, one file or all of them.

Only `product/` is checked out and only files under it can be written.
It works through the GitHub API, so the control plane needs no git binary.

**If the base branch deploys** (merging to `main` ships the app), make its CI
ignore product-only commits, or every saved story map triggers a release:

```yaml
on:
  push:
    branches: [main]
    paths-ignore: ["product/**"]
```

## Layout in the repository

```
product/
  README.md                         what the product is, for whom, what good looks like
  *.md                              any other product docs
  user-story-maps/
    <map>/
      map.yaml                      personas, the journey, releases, metrics
      tasks/
        <user-task>.yaml            one file per user task
```

Flows read these files for context (the brief tells them where), but they do
not edit them; the product owner does.

### map.yaml

```yaml
title: Arcade
description: Casual browser games anyone can pick up.
personas:
  - id: player
    name: Casual player
    description: plays on a phone during a break
journey:                    # phases, left to right
  - id: discover
    title: Discover
    activities:             # the backbone: big things users do
      - id: browse
        title: Browse games
        persona: player
  - id: play
    title: Play
    activities:
      - id: play-game
        title: Play a game
releases:                   # slices, top to bottom
  - id: mvp
    title: MVP
    goal: one great game loop
    status: in_progress     # planned | in_progress | released
metrics:
  - id: weekly-players
    title: Weekly players
    kind: product           # a PromQL query against the metrics backend
    query: sum(increase(games_started_total[7d]))
    target: 100
    direction: up           # which way is better
  - id: mvp-done
    title: MVP done
    kind: delivery          # computed by ai-flow
    measure: done_ratio     # tasks_done | done_ratio | cycle_time_hours | run_cost_usd | run_success_rate
    release: mvp
    unit: "%"
```

### tasks/<user-task>.yaml

```yaml
title: See every game on the home page
activity: browse            # an activity id
release: mvp                # a release id; omit for Unscheduled
order: 1                    # position within its activity and release
persona: player             # optional; defaults to the activity's persona
story: As a player I want to see all games at once so that I can pick one quickly.
acceptance:
  - every game has a tile with a title and picture
  - the grid works on a phone
metrics: [weekly-players]   # metrics this task should move
status: todo                # todo | in_progress | done | blocked
```

Ids are lowercase slugs and become file names. A map with dangling references
(a task whose activity was removed, say) still loads; the problems are listed
above the map.

## Working on a map

- The grid shows phases and activities across the top and releases down the
  side; user tasks sit in the cell for their activity and release. Drag a card
  to move or reorder it, or change its activity and release in its panel.
- **Edit map** edits `map.yaml` as YAML. The `+` buttons add phases,
  activities, releases and tasks.
- **Assistant** answers questions about the map and can propose changes. Each
  proposal is checked against the map (and retried once if invalid), shown as
  a diff, and only written when you press **Apply**. It is still not committed.
- **Metrics** shows product metrics (current value and the last 7 days) and
  delivery metrics, against their targets.

## Sending work to a flow

A user task, an activity (its open tasks) or a phase (the open tasks of all
its activities), optionally limited to one release, can be sent to a flow.
ai-flow creates a task whose description is a brief: each user task's story,
acceptance criteria, journey position, persona, release goal and metrics, plus
a pointer to `product/`. The planner turns it into a flow as usual, and the
project's start mode decides whether it starts by itself.

Each card shows the latest work covering it: planning, flow ready, running,
waiting for you, succeeded or failed, linked to the run. The `status` in the
task file changes only when you change it (the panel offers "mark done" once a
flow succeeds), and only reaches the repository when you commit.

## Run review

On a run's page, **Review** asks the planner model to read the run's record
(flow, task, every step with its outcome, error, duration, cost, outputs and
log tail, and the events) and say what went well, what went wrong, and what to
change: the flow, catalog presets, prompts, models, config, or ai-flow's own
primitives. You can give it something to focus on. Reviews are stored with the
run and can be redone.

## Where configuration lives

On its first start, the server copies the catalog and projects from its config
files into its database and uses the database from then on: **Settings** edits
models, node presets, runtimes, harnesses, access grants, skills, planner
settings and projects. Every change is checked against the whole config before
it applies, takes effect immediately without a restart, and warns about saved
flows it breaks. The environment (endpoints, secrets, runtime settings) still
comes from the config files.

The files keep working for releases: the server remembers the files as it last
read them, and on each start every entry that changed in the files since then
(a runtime image bumped by a release, a preset updated by a catalog sync, a
project) replaces that entry in the database. Entries the files did not change
keep their UI edits; when both changed the same entry, the files win. If the
result would be invalid (the files' planner names a model deleted in the UI,
say), nothing from the files is applied, the database version is served, and
**Settings** shows why until it is resolved.

To start over from the files, delete the `config/` keys from the `kv` table
and restart.
