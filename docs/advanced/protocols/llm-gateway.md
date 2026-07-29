<!-- nav: AI gateway -->

# AI gateway inlet — `POST /v1/messages`

_Operator and author reference for the AI-gateway inlet: routes, credential modes, the
`_llm` envelope contract, and the flags that bound it. The concept page is
[AI Gateway](../../gateway.md)._

The inlet speaks the Anthropic Messages protocol to clients, runs each request once
through the tenant's `_llm` stack, and forwards the result upstream. The gateway owns the
transport: a stack that streams a response is an error, and the upstream stream is never
routed through the processor.

## Routes

Both are registered on the web head ahead of the catch-all, so they bypass BasicAuth and
the opstack render path. The `web` personality must be enabled.

| Route | Stack runs | Metered | Notes |
| --- | --- | --- | --- |
| `POST /v1/messages` | yes | yes | The policy surface. Completion envelope fires afterwards. |
| `POST /v1/messages/count_tokens` | no | no | A metadata echo clients call constantly — forwarded transparently. Tenant routing, the `_llm` self-gate, and auth still apply. |

The client's path **and query string** are forwarded verbatim — clients append
`?beta=true`, and dropping the query would silently change protocol behavior.

## Opt-in and routing

1. **Host → tenant**, through the same DB-backed hostname resolver the web pipeline uses.
   A resolver *failure* is a 503 with `Retry-After: 1`, never a fabricated 404.
2. **Self-gate**: one point read asks whether that tenant authored an `_llm` stack. No
   stack, no gateway — the hostname gets `404 not_found_error` on these paths.

So enabling the gateway for a tenant is exactly: bind a hostname, author `OPS/_llm/`,
apply. There is no `--llm-enable` flag.

## Credential modes

Chosen per request by what the tenant's [secret store](../runbook-secret-store.md) holds,
scoped to the `_llm` stack (stack-scoped falls back to tenant-wide).

| Stored | Mode | Behavior |
| --- | --- | --- |
| neither | `passthrough` | The client's own `x-api-key` is forwarded verbatim. |
| `ANTHROPIC_KEY` + `LLM_GATEWAY_KEY` | `swap` | The inbound `x-api-key` is compared to `LLM_GATEWAY_KEY` in constant time; on match it is replaced with the real key and any client `Authorization` header is dropped. |
| `ANTHROPIC_KEY` alone | fails closed | **401 for every client**, plus a warning log. The real key is never spent on an unauthenticated request. |
| secret-store *error* | — | 503. The mode is unknowable, so neither is served. |

The mode in effect is readable in the stack as `@llm.auth_mode`.

:::note
These two secrets have **no environment-variable fallback**. Unlike `OPENROUTER_KEY` and
`OPENAI_KEY` (see [ai.md](../../ai.md#set-the-api-key), governed by
`--ai-chat-env-fallback`), they are read only from the secret store — so an operator's own
exported `ANTHROPIC_API_KEY` can never become a tenant's upstream credential. Note the
name: the secret is `ANTHROPIC_KEY`; `ANTHROPIC_API_KEY` is the *client's* variable,
forwarded in passthrough mode.
:::

## The `_llm` contract

Envelopes arrive with `_txc.src == "llm"` and route to `_llm/0`. `@` is sugar for
`._txc.`, so `@llm.phase` is `_txc.llm.phase`.

### Stamped on the request (read-only)

Everything under `_txc` is chassis-stamped; the client's bytes are confined to the
top-level `request` key, so a hostile client cannot forge control state.

| Field | What |
| --- | --- |
| `@llm.phase` | `"request"` |
| `@llm.protocol` | `"anthropic.messages"` |
| `@llm.request_id` | Correlates the request and completion runs |
| `@llm.tenant`, `@llm.host`, `@llm.hostname_verified` | Resolved routing |
| `@llm.stream` | Whether the client asked for SSE |
| `@llm.auth_mode` | `"passthrough"` or `"swap"` |
| `@llm.req.user_agent`, `@llm.req.anthropic_version`, `@llm.req.anthropic_beta` | Present only when the client sent them |
| `request` | The client's Messages JSON — mutable |

### Writable by the stack

These four subtrees are the entire author-writable surface under `_txc.llm`. The
[system context](../context.md) is default-closed to authors, so anything else under
`@llm` is reserved. Use `EMIT` — `SET` does not persist into the envelope the gateway
reads back.

| Path | Effect |
| --- | --- |
| `@llm.reject.{status,type,message}` | Answer without contacting the upstream. A status outside 100–599 becomes `403`; an empty type becomes `permission_error`. |
| `@llm.upstream.url` | Override the base URL for this request. Must be `http`/`https` with a host, and is subject to the egress policy; anything else is a 500. |
| `@llm.headers` | String-valued headers set on the upstream request. |
| `@llm.context` | Items for the gateway to inject — see below. |

Rewriting the outbound request itself is a plain `EMIT .request.…`.

### The completion envelope

Fires asynchronously into the same stack after the exchange ends, with a **new** `rid`
(reusing the request's would overwrite its trace) and `@llm.request_id` carrying the
original for correlation.

| Field | What |
| --- | --- |
| `@llm.phase` | `"completed"` |
| `@llm.completion.{status,duration_ms,bytes_in,bytes_out}` | The exchange. `status` is `0` when the dial failed. |
| `@llm.completion.{requested_model,effective_request_model,response_model}` | Explicit provenance: the client's ask, what went upstream, what was served. `response_model` is capture-dependent. |
| `@llm.completion.{stream,upstream,client_disconnected}` | How it ran |
| `@llm.completion.error` | Present only on failure |
| `@llm.completion.usage.{input_tokens,output_tokens,cache_read_input_tokens,cache_creation_input_tokens}` | **Presence-gated** — absent means "not captured", never zero |
| `@llm.completion.stop_reason` | Present when captured |
| `@llm.context_result` | Gateway-owned; see below |

:::warning
Gate every rule on `@llm.phase`. The completion envelope routes into the same stack, so an
ungated request-shaping rule runs a second time against it.
:::

Token capture is a passive tee over the same bytes the client receives, so it never
delays the stream and never retains content. Streaming policy requests are forwarded with
`Accept-Encoding: identity` so the SSE frames stay parseable; the client still sees an
unencoded stream under its own negotiation.

## Context injection

The stack emits `@llm.context` as an array of `{source?, title?, content}`; the gateway
validates, dedups, budget-fits, and serializes the survivors into system blocks. Policy
stays in txcl, serialization stays in Go — a stack cannot reliably write `request.system`
itself, because a merge drops an emitted array when the envelope's `system` is a string.

- Blocks are appended **after** the client's own system content, preserving its cached
  prefix, behind a chassis-owned guard block declaring the material untrusted evidence
  rather than instructions. That guard is not stack-writable and counts against the budget.
- Items first-fit under both caps after the guard is reserved: a too-large item is
  dropped and a later smaller one may still fit. If nothing fits, nothing is injected —
  never a bare guard.
- Deduplication is by content sha256 and by `(source, title)`. Labels are stripped of
  control characters and capped at 200 bytes.
- The token estimate is `bytes/4` — a guardrail, not an invoice. Exact counts arrive
  later on the completion envelope.

`@llm.context_result` is the ground truth, one row per emitted item plus a synthetic guard
row: `{source, title, sha256, bytes, est_tokens, status, synthetic}` where `status` is
`injected`, `dropped_budget`, `dropped_dup`, or `invalid`. Rows carry sha256 and byte
counts, **never content** — traces must not become a second copy of organizational memory.
Stacks cannot write this path.

:::warning
Injection is enabled only when **both** `--llm-context-max-tokens` and
`--llm-context-max-items` are positive. Either at `0` disables it entirely, and the only
signal is a debug log — emitted items are silently ignored.
:::

## Flags

Every flag is also an environment variable with the `TXCO_` prefix and underscores
(`--llm-upstream-url` ↔ `TXCO_LLM_UPSTREAM_URL`) — see [serve.md](../serve.md).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--llm-upstream-url` | `https://api.anthropic.com` | Base URL forwarded to after the stack runs. Point at a local fake for testing. Invalid at boot is a hard start failure. |
| `--llm-context-max-tokens` | `2000` | Estimated-token cap per request, guard block included |
| `--llm-context-max-items` | `8` | Item cap per request |

Adjacent flags the inlet consumes:

- `--op-timeout-max` (default `10m`) — ceiling on the stack round-trip; falls back to
  `--web-write-timeout` if unparseable. A single blocked client write is bounded at 60s
  while total stream length stays unbounded.
- `--web-max-body-bytes` (default 30 MiB) — request-body cap, enforced before admission;
  over it is a `413 request_too_large`.
- `--egress-policy` (default `private`) — applies at dial time to the configured upstream
  **and** to any `@llm.upstream.url` override, so a stack-supplied URL gets no more reach
  than any other outbound call.

## Errors

Gateway-originated errors use the protocol's own shape, so clients surface them natively:

```json
{"type": "error", "error": {"type": "permission_error", "message": "…"}}
```

The types the gateway itself emits are `invalid_request_error`, `authentication_error`,
`permission_error`, `not_found_error`, `request_too_large`, `rate_limit_error`, and
`api_error`. **Upstream error bodies are never rewritten** — the client sees exactly what
the upstream said, including its status.

Admission denials map by status: `429` → `rate_limit_error`, `5xx` → `api_error`,
otherwise `permission_error`, with `Retry-After` passed through when the gate suggested
one. An upstream dial failure is `502 api_error`; the stack failing to run, leaving no
`request` object, or streaming a response is `500 api_error`.

## Metering

One usage event per `/v1/messages` request records `src: "llm"`, `stack: "_llm"`,
duration, bytes in/out, and `billable: true`. That is transfer metering — stack
[fuel](../fuel.md) is metered separately by the pipeline, and token counts are
record-only: nothing is charged from them today. `count_tokens` records nothing.

## See also

- [AI Gateway](../../gateway.md) — the concept page
- [AI Operations](../../ai.md) — the outbound direction, `ai://chat` and `ai://embed`
- [Chassis runtime](../serve.md) — the full flag reference
- `examples/llm-gateway-hello/` — a runnable stack, keyword and vector retrieval
