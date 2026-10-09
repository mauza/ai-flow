# Running ai-flow locally in kind

This sets up the whole system on one machine: the control plane, the step pods
(each a Kubernetes Job), Garage for transcripts and a demo MCP server, all in a
[kind](https://kind.sigs.k8s.io/) cluster. It is the same Helm chart a real
cluster uses, so isolation and network policies behave as they do in production.

Want something lighter? `make dev-local` runs the control plane on your machine
and each step as a child process: no cluster, no isolation, quick to iterate.

## What you need

- **An x86-64 Linux or macOS machine** with Docker running. The images are built
  for amd64 only; arm64 hosts (Apple silicon) are not supported yet.
- **Tools:** `kind`, `kubectl`, `helm`, Go 1.26+, Node 22+ with npm, and the
  GitHub CLI `gh`.
- **A GitHub token** that can read and write the repositories you want flows to
  work on (contents, pull requests and, for release flows, actions). By default
  ai-flow uses `gh auth token`, so `gh auth login` is enough.
- **An OpenAI-compatible model endpoint**: OpenAI itself, OpenRouter, a LiteLLM
  gateway, vLLM, llama.cpp or Ollama. Agents need a model that calls tools well.
- **A throwaway GitHub repository** to try flows on. Flows push branches and open
  pull requests there.

## 1. Configure

```sh
git clone https://github.com/mauza/ai-flow && cd ai-flow
cp .env.example .env
```

Fill in `.env`:

| Variable | What it is |
|---|---|
| `LLM_BASE_URL` | Your model endpoint, ending in `/v1`. Empty means `https://api.openai.com/v1`. |
| `LLM_API_KEY` | Its key, if it needs one. |
| `GITHUB_TOKEN` | Optional; defaults to `gh auth token`. |
| `LINEAR_API_KEY` | Optional; see [Linear](#linear-optional). |

**The endpoint must be reachable from inside the cluster.** `localhost` there
means the pod itself, not your machine. For a model server on your machine, use
the address of the kind Docker network's gateway, and make the server listen on
all interfaces (for Ollama: `OLLAMA_HOST=0.0.0.0 ollama serve`):

```sh
docker network inspect kind -f '{{range .IPAM.Config}}{{.Gateway}} {{end}}'
# e.g. 172.18.0.1  ->  LLM_BASE_URL=http://172.18.0.1:11434/v1
```

The `kind` network exists after the first `make dev-up` (you can fill in
`LLM_BASE_URL` then and apply it, see [Day to day](#day-to-day)). An endpoint
elsewhere (a server on your network, a VPN address, a hosted API) works as is.

**Linux firewalls** (ufw, firewalld) usually drop traffic from Docker networks
to services on your machine, while traffic going out is fine. Allow the model
server's port from the kind network, for example with ufw and Ollama's port:

```sh
sudo ufw allow in on $(docker network inspect kind -f 'br-{{slice .Id 0 12}}') to any port 11434 proto tcp
```

Only the control plane talks to the model endpoint. Step pods reach nothing
but the control plane, which proxies their model calls.

## 2. Start the cluster

```sh
make dev-up
```

This:

1. creates a kind cluster named `ai-flow` (if it does not exist) with the UI on
   `localhost:8080` and a state folder in `~/.local/share/ai-flow/ai-flow`;
2. builds the UI, the binary and three images (control plane, agent runtime,
   agent runtime with Go) and loads them into the cluster;
3. copies `.env` into the `ai-flow-secrets` Secret;
4. installs the Helm chart with the config in [`deploy/config`](../deploy/config).

The first run takes several minutes, mostly building images. Then open
<http://localhost:8080>.

Options: `UI_PORT=8090 make dev-up` if port 8080 is taken, and
`CLUSTER=other make dev-up` for a second, separate cluster (its own state folder
and kube context). The Makefile always passes `--context kind-<cluster>`, and
creating the cluster switches your current kube context back afterwards, so
commands meant for other clusters are not redirected.

## 3. Match the models to your endpoint

The example catalog names three models (`gpt-6.1-sol`, `gpt-6-luna`,
`gpt-6-astra`). If your endpoint does not serve those names, open
**Settings → Models** and either edit each entry's `model:` to a model your
endpoint serves, or add your own models. When you add new names, also:

- set **Settings → Planner → planner** `model:` to one of them, and
- list them under `allow.models` in **Settings → Projects** for the projects
  that may use them (an empty list allows every model).

Changes apply immediately, after a validation check.

## 4. Point it at a repository

Pick one:

- **Products → Link a repository** lists the repositories your token can see.
  Linking one adds a project with read and write access to it.
- Or reuse the sample `sandbox` project: in **Settings → Access**, edit
  `repo/ai-flow-sandbox` and set `url` to your throwaway repository.

## 5. Run something

1. **Board → New task**, pick the project and describe a small change ("add a
   `hello()` function with a test").
2. The planner drafts a flow; open it from the task to see the graph.
3. Press **Run**. The run page shows each step live: pods starting, the agent's
   progress, test results, and finally a pull request in your repository.

For a quicker check that pods, the git proxy and reporting work, save this as a
new flow (replace `<project>`) and run it:

```yaml
apiVersion: ai-flow/v1alpha1
kind: Flow
metadata: { name: smoke, project: <project> }
spec:
  start: look
  nodes:
    look:
      type: check
      run: git log --oneline -1 && ls
      next: { pass: $success, fail: $fail }
```

From there, [docs/PRODUCT.md](PRODUCT.md) covers product docs and story maps,
and [docs/TEMPLATES.md](TEMPLATES.md) covers reusable flows.

## Day to day

| Task | Command |
|---|---|
| Rebuild and roll out after changing code or `deploy/config` | `make dev-reload` |
| Apply a changed `.env` | `make secrets`, then `kubectl --context kind-ai-flow -n ai-flow rollout restart deploy/ai-flow` |
| Follow the control plane's log | `make logs` |
| UI with hot reload | `cd web && npm run dev` (proxies to `localhost:8080`; set `AI_FLOW_API=http://localhost:8090` for another port) |
| Stop the cluster, keep its state | `make dev-down` |
| Delete the cluster and its state | `make dev-reset` |

State (the SQLite database, transcripts) lives in the state folder, so
`make dev-down` and a later `make dev-up` pick up where you left off.

**Where config lives after the first start.** On first start the server copies
the catalog and projects from `deploy/config` into its database, and Settings
edits them from then on. Editing `deploy/config` still works: after
`make dev-reload`, every entry you changed there replaces that entry in the
database, and every other entry keeps its Settings edits. The environment
(`deploy/config/environment.yaml`) always comes from the file.

## Linear (optional)

To start tasks from Linear issues:

1. Put `LINEAR_API_KEY` in `.env` and run `make secrets`.
2. Set `linear.enabled: true` in `deploy/config/environment.yaml`.
3. In the project's `linear` block (in `deploy/config/project-sandbox.yaml`, or
   in **Settings → Projects** once running), set your team key, the trigger
   label and states, and the states ai-flow moves issues to.
4. `make dev-reload`.

Polling needs no public URL. Webhook mode does, so it is not useful in kind.

## Troubleshooting

- **Planning fails with HTTP 401.** The endpoint needs a key: set `LLM_API_KEY`,
  then `make secrets` and restart the deployment (see Day to day).
- **Planning fails with "connection refused" or a timeout.** `LLM_BASE_URL` is
  not reachable from the cluster (see step 1, including the firewall note).
  Check from inside the cluster, with your URL in place of the example:
  `kubectl --context kind-ai-flow -n ai-flow run probe --rm -it --image=curlimages/curl --restart=Never -- curl -s -m 5 http://172.18.0.1:11434/v1/models`
- **"model not found" from the endpoint.** The catalog's `model:` names do not
  exist there; see step 3.
- **A step fails while cloning, or a push is refused.** The token cannot read or
  write that repository, or the grant's `url` is wrong.
- **`make dev-up` fails binding port 8080.** Something else uses it. The port is
  fixed when the cluster is created: `make dev-down`, then
  `UI_PORT=8090 make dev-up`.
- **A step's first network call fails in its first seconds.** kind's network
  plugin applies NetworkPolicy a few seconds after a pod starts; steps that probe
  the network right away can see that gap. Real clusters with Calico or Cilium
  do not have it.
- **Something else.** `make logs` shows the control plane; a step's pod logs are
  in the `ai-flow-runs` namespace, and the run page shows each step's output and
  transcript.
