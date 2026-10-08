# Astra, Sol and Luna through a gateway-owned ChatGPT login

LiteLLM owns the subscription session and refreshes it. OpenCode and ai-flow are
clients of the gateway; neither needs to renew that session or distribute tokens.

```text
OpenCode ─┐
          ├─ LiteLLM ─ ChatGPT Responses API
ai-flow ──┘      │
                └─ persistent access + refresh credentials
```

The selectable ai-flow models are **`gpt-6-astra`**, **`gpt-6.1-sol`**, and
**`gpt-6-luna`**, with no fallback to other families. Sol remains the default
streaming planner. OpenCode defaults to `litellm-openai/gpt-6-astra` and uses Luna
for its small/background-model setting. Existing local-model configuration in
OpenCode is preserved separately.

## Routing and clients

`home-k8s/apps/llm-gateway/litellm.yaml` registers each model with
`model: chatgpt/<model-name>`, `drop_params: true`, and **`model_info.mode:
responses`**. This is required with the installed LiteLLM 1.82.6: unknown new IDs
otherwise take the wrong upstream endpoint. LiteLLM exposes an OpenAI-compatible
`/v1/chat/completions` endpoint and bridges requests to ChatGPT's Responses API.

Clients stream. The ai-flow planner and structured `llm` steps collect the stream;
pi agents and OpenCode already stream. The client accepts LiteLLM's empty-choice
usage trailer after the stop marker, while rejecting additional generated content.

OpenCode uses a **distinct provider ID**, `litellm-openai`, with
`@ai-sdk/openai-compatible` and `http://litellm:4000/v1`. Its built-in `openai`
provider is disabled. Overriding the native provider's base URL would be wrong:
its OAuth hook can rewrite requests back to the direct Codex endpoint.
The gateway requires a client key: ai-flow sends `LITELLM_API_KEY` (the
upstream's `apiKeyEnv`; in the cluster it lives in `ai-flow-secrets`, locally
in `.env`), and OpenCode needs its own key. It is a gateway key, not an
OpenAI credential.

**Restart OpenCode after configuration changes.** A resumed conversation may
retain its old model choice; select one of:

- `litellm-openai/gpt-6-astra`
- `litellm-openai/gpt-6.1-sol`
- `litellm-openai/gpt-6-luna`

The subscription backend may drop token-limit parameters. Observed usage still
drives ai-flow's local limits, but a requested output limit is not necessarily an
upstream hard cap. Subscription accounting does not imply unlimited quota or
separately paid OpenAI API credit. Gateway clients share the account's entitlements.

## Refresh ownership and persistence

The active auth file lives at `/var/lib/litellm/chatgpt/auth.json` on the
`llm-gateway/litellm-chatgpt-auth` PVC (`nfs-client`). Directory/file permissions
are 0700/0600. `CHATGPT_TOKEN_DIR` points there, allowing the native authenticator
to save rotated credentials. Refresh occurs on demand when access is near expiry;
there is no workstation sync or recurring refresh script.

The gateway runs one replica and one worker, using `Recreate` deployments to avoid
overlapping credential owners during a rollout. This makes gateway rollouts
briefly unavailable. Do not scale replicas/workers without revisiting shared
refresh coordination. The PVC has ArgoCD `Prune=false,Delete=false` protection so
an application/manifest rollback does not automatically erase the current login.

The init container seeds **only a new, empty volume** from the old access-only
Secret. It never overwrites an existing auth file. That Secret is a bootstrap
artifact, not the refresh owner or a backup of rotated credentials. Losing the
PVC requires restoring its current auth data or authenticating again; the stale
bootstrap access token cannot recover the refresh session.

## One-time handoff

`scripts/handoff-opencode-chatgpt.py` securely transfers an existing OpenCode
session, without printing credentials or placing them in command arguments:

1. Validate current access, account identity and a usable refresh credential.
2. Stage the record privately on the gateway PVC without making it active.
3. Atomically replace only OpenCode's refresh field with an empty string,
   preserving unrelated provider entries and the active session's access token.
4. Activate the staged gateway record. LiteLLM is then the refresh owner.

Stop the previous sync timer/service and other login/auth writers before handoff.
The script refuses an active sync timer, an `OPENCODE_AUTH_CONTENT` override,
detected concurrent source changes, or a destination that already owns refresh.
It does not overwrite an existing gateway login with a stale source copy.

```sh
# Validate source only.
python3 scripts/handoff-opencode-chatgpt.py

# One-time transfer after preparing the gateway and OpenCode configuration.
python3 scripts/handoff-opencode-chatgpt.py --apply

# Optional verification: deliberately performs one real native OAuth refresh.
python3 scripts/handoff-opencode-chatgpt.py --apply --verify-refresh
```

The verification exercises the native expired-access branch in an isolated
process, disables interactive login fallback for that test, and checks that the
refreshed session was saved. It returns only a fresh **access-only** bridge to the
still-running native OpenCode session. The refresh token never returns locally.
This is a migration check, not a scheduled job.

The transitional local OAuth entry has `refresh: ""`. It lets the current session
finish using valid access but cannot renew it. After restarting OpenCode and
selecting the gateway model, optionally remove that leftover entry:

```sh
python3 scripts/handoff-opencode-chatgpt.py --apply --retire-local-access
```

Do not run this final cleanup in the still-running native session: its loader
expects the entry to exist. If handoff is interrupted, inspect gateway ownership
and the private `.handoff.json` staging file before retrying. Never restore an old
refresh copy over credentials that LiteLLM may already have rotated.

## Verification and rollback

On 2026-09-28:
- Astra, GPT-6 Sol (the previous version) and Luna worked through the native ChatGPT Responses bridge.
- LiteLLM's native refresh successfully renewed and persisted the transferred
  session without a new login.
- The previous five-minute workstation sync was disabled.
- OpenCode's separate gateway provider completed real tool-call round trips.
- GPT-6 Sol generated a valid bounded test/review flow on its first attempt. GitHub's
  sandbox context lookup returned 404, so that test did not use fetched repository
  context or execute the resulting flow.

### GPT-6.1 Sol upgrade

GPT-6.1 Sol replaces the previous Sol choice as `gpt-6.1-sol`. The published
gateway no longer lists `gpt-6-sol`. Live ai-flow exposes only Astra, GPT-6.1 Sol
and Luna, with GPT-6.1 Sol as its streaming planner. OpenCode exposes
`litellm-openai/gpt-6.1-sol`; restart OpenCode to load the updated choice.

The new upstream returned its exact model ID in a streaming smoke test. OpenCode
completed a real read-tool round trip through the published route. The deployed
ai-flow binary generated a valid bounded test/review workflow in two planner
attempts, with Luna reviewing the change. The generated flow was not executed.
Both ArgoCD applications were Synced/Healthy after rollout.

Model/catalog configuration can roll independently of application images.
Application UI/parser changes require their own image/chart release; changing
gateway routes alone does not deploy application code.

Rollback must preserve the PVC's **current** credentials. Reverting to the earlier
access-only mount/timer alone cannot recover refresh ownership. To move ownership
back, first stop gateway credential writes, securely transfer its current record
to the intended client, and retire gateway refresh; do not run two refresh owners.

Sources:
- [LiteLLM ChatGPT provider](https://docs.litellm.ai/docs/providers/chatgpt)
- [OpenCode OpenAI OAuth implementation](https://github.com/anomalyco/opencode/blob/v1.18.30/packages/opencode/src/plugin/openai/codex.ts)
- [OpenAI authentication](https://developers.openai.com/codex/auth)
