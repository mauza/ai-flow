# ai-flow: agent notes

Per-task agent flows on Kubernetes: one Go binary (API + embedded React UI, state-machine
engine, broker, planner, `ai-flow node` pod runner); each step runs as a Job in
`ai-flow-runs`; pi is the default harness (`pi-ext/`), opencode and codex are pluggable.
Public repo (GPL-3.0): code and docs stay generic; this file is the one place for the
home install. Never put credentials or tokens here.
Design: `docs/SPEC.md`; reliability: `docs/RELIABILITY.md`; product layer: `docs/PRODUCT.md`.

Prod: **http://ai-flow:8080** (tailnet-only, no app auth), namespace `ai-flow`, Argo app
`ai-flow`. Prod config lives in home-k8s `apps/ai-flow/` (read its README; don't copy it here).

## Build and test

- `make test`, `make test-race`, `make lint` (vet + helm lint), `make test-templates`.
- UI: `cd web && npm ci && npm run build`; browser tests:
  `PLAYWRIGHT_CHROMIUM_EXECUTABLE=/usr/bin/chromium npm test` (bundled browser is missing here).
- CI (`.github/workflows/ci.yml`) runs on every push and PR: tests, race, vet, evals, helm,
  and `git diff --check HEAD^ HEAD` (no trailing whitespace). CI builds no images.
- `make dev-local` runs everything on the host. kind (`make dev-up`) is optional and
  currently unused; ufw on this desktop blocks kind pods from reaching host ports.

## Ship it

Verified changes ship without asking: commit, merge to `main`, push, release, deploy, and
check it live (steps below). Ask first only for data loss or a deploy that would fail
active runs.

## Release and deploy (no Image Updater; done by hand)

1. Merge to `main` (feature branch + PR, or a direct push), push, CI green.
2. Release ONLY from the `~/Projects/ai-flow-release` worktree (it has `main` checked out;
   `~/Projects/ai-flow` is for feature branches). Switching branches mid-release once
   tagged images with an unmerged commit.
   `cd ~/Projects/ai-flow-release && git pull --ff-only && make release REGISTRY=docker.mau.guru/library`
   pushes `ai-flow`, `ai-flow-agent`, `ai-flow-agent-go`, `ai-flow-agent-web` tagged with
   the short sha (`git describe --always`); it refuses a dirty tree.
3. Before deploying: `curl -s http://ai-flow:8080/api/runs | jq -r '.[].status' | sort | uniq -c`.
   A restart interrupts in-flight planning; a change to the snapshot fingerprint format
   fails active runs, so ship that only with no queued/running/waiting runs. Agent steps
   survive a restart.
4. If presets, skills, harnesses or defaults changed: in home-k8s
   `apps/ai-flow/sync-catalog.py ~/Projects/ai-flow-release`, review `git diff`, ship it in
   the same commit as the pins.
5. Bump pins in home-k8s, all in ONE commit (config needing a new binary must not land
   before it). Read the current pins first, never reuse the sha you last deployed:
   ```sh
   cd ~/Projects/home-k8s
   grep -n targetRevision argo/apps/ai-flow.yaml; grep -n 'tag:' apps/ai-flow/values.yaml
   new=$(git -C ~/Projects/ai-flow-release rev-parse HEAD)
   bump-pin <old-full-sha> $new argo/apps/ai-flow.yaml        # chart, full sha
   ship-pin -a ai-flow -m "ai-flow <short>: <why>" <old-short> <short> \
     argo/apps/ai-flow.yaml apps/ai-flow/values.yaml apps/ai-flow/config.yaml
   ```
   Full sha first (the short-sha replace would mangle it); `ship-pin` then commits all three
   files: chart `targetRevision`, `image.tag`, the 3 runtime images in `config.yaml`.
   Keep `image.tag` quoted: an all-digit sha became a YAML number (the chart now rejects it).

## Verify live

- `ship-pin` ends with `argo-wait ai-flow` (Synced+Healthy, running images); then
  `curl -s http://ai-flow:8080/healthz` → `ok`, and
  `k -n ai-flow get deploy ai-flow -o jsonpath='{..image}'` shows the new tag.
- Config merge: pod log `took entries ...` lists applied config.yaml entries; conflicts show as `files_error`.
- A sealed-secret change can land after the pin bump restarted the pod: compare times,
  then `k -n ai-flow rollout restart deploy/ai-flow`.

## Traps (still true)

- SQLite is on `nfs-client` on purpose, an exception to the cluster rule: safe only because
  `internal/store` sets `locking_mode(EXCLUSIVE)` before `journal_mode(WAL)`. Keep that order,
  1 replica and Recreate. After a node dies the new pod waits ~90 s for the NFS lease.
- Catalog and projects live in the DB (UI-editable); home-k8s config.yaml entries still
  replace their DB entry when they change. Reseed = delete the `config/` kv keys.
- Images are amd64-only; chart and `runs.nodeSelector` pin amd64 (5 nodes are arm64).
- Step pods can't `git fetch/pull/push`: only the runner's git commands carry git-proxy creds.
- Flow YAML is parsed by `sigs.k8s.io/yaml`: outcomes `yes`/`no` become booleans; quote them.
- `flow.NodeIDs` must list parallel branches before their join: the UI's ELK layout uses
  MODEL_ORDER, and the join renders beside the fork otherwise (covered by a test).
- Prod GITHUB_TOKEN is a fine-grained PAT: it can't read Checks, so CI comes from Actions jobs.
- Stacked PRs: merging the base with `--delete-branch` closes the stacked PR; retarget first.
- Inside an ai-flow step (any project): the flow owns commits, PRs, merges and deploys.
