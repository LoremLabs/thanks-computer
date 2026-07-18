# llm-gateway-hello

Point an existing AI client (Claude Code, or plain `curl`) at your chassis
instead of Anthropic. The **AI-gateway inlet** answers `POST /v1/messages`
(Anthropic Messages protocol), runs the request through this workspace's
`_llm` stack — which can reject it, rewrite the model, or add headers — then
forwards it upstream and streams the SSE response back byte-transparent. The
stack runs **once per request**; the stream never passes through it.

Authoring an `_llm` stack IS the opt-in: hostnames without one 404 on the
same path.

## Run it (no upstream needed)

```sh
txco dev          # in this directory (auto-applies the stacks AND, since
                  # `app` is this workspace's only web stack, auto-binds
                  # plain localhost → app, so Host→tenant routing resolves)

# The guard rule rejects this model before any upstream is contacted:
curl -sN http://localhost:8080/v1/messages \
     -H 'content-type: application/json' \
     -d '{"model":"txco-test-reject","max_tokens":8,"messages":[]}'
# → {"type":"error","error":{"type":"permission_error","message":"blocked by _llm policy"}}
```

(Multi-stack workspaces don't auto-bind — there the bind is explicit:
`txco auth tenant hostnames add localhost --stack <stack>`. The gateway
only needs the tenant, so ANY bound stack makes /v1/messages live.)

`txco trace` shows the request-phase pipeline run: the guard's WHEN firing,
the EMIT verdict, and (on forwarded requests) the completion-phase run that
follows the stream.

## Against the real Anthropic API

The dev chassis defaults `--llm-upstream-url` to `https://api.anthropic.com`.
Two credential modes, decided per tenant by what's in the secret store:

- **Passthrough** (nothing stored): the client's own `x-api-key` is forwarded
  verbatim. Zero setup — the curl below works with your key.
- **Swap** (store `ANTHROPIC_KEY` + `LLM_GATEWAY_KEY`): clients authenticate
  with the gateway key; the real key never leaves the server.

```sh
# Passthrough:
curl -sN http://localhost:8080/v1/messages \
     -H 'Host: localhost' -H 'content-type: application/json' \
     -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' \
     -d '{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

The policy rule pins `request.model` to `claude-haiku-4-5-20251001` — the
upstream receives the rewritten model regardless of what the client asked
for, and the response streams back incrementally. (If Anthropic retires
that id, update the pin: a nonexistent model makes the upstream 404 every
request, and clients misattribute the passthrough error to their own
selected model.)

## Claude Code through the gateway

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 claude
```

(Keep your normal `ANTHROPIC_API_KEY` in passthrough mode; use the tenant's
`LLM_GATEWAY_KEY` value in swap mode.) A coding session behaves identically
to talking to Anthropic directly — while every request runs through
`OPS/_llm` and lands in the trace. `/v1/messages/count_tokens` (which
Claude Code calls constantly) is forwarded transparently WITHOUT the stack —
it's a metadata echo, not a policy surface.

Note: with the policy rule active, every request is pinned to
`claude-haiku-4-5-20251001` — Claude Code will report whatever model it asked
for, but the upstream serves the pinned one. Delete `OPS/_llm/100/policy.txcl`
(and `txco apply`) for a fully transparent session.

## The three rules

| File | Phase | What it shows |
|---|---|---|
| `OPS/_llm/50/guard.txcl` | request | EMIT `@llm.reject` → Anthropic-shaped error, upstream never contacted |
| `OPS/_llm/100/policy.txcl` | request | rewrite `.request.model`, tag the upstream call via `@llm.headers` |
| `OPS/_llm/200/completed.txcl` | completed | react to `@llm.completion.*` (status, duration_ms, bytes in/out) after the stream ends |

Rules must gate on `@llm.phase` — the completion envelope routes into this
same stack, and an ungated request-shaping rule would re-run on it.
