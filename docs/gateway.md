# AI Gateway — point your AI client at the chassis

_In [Thanks, Computer](https://www.thanks.computer), your AI client's own requests become
[operations](./ops.md): a stack sees every one before it leaves, and can answer it with
context the model didn't have._

Ask your coding agent "why did we choose boltdb over redis?" and it answers from your
architecture notes — not because you pasted them in, but because the request passed
through a stack you author, which found the relevant note and attached it before the
model ever saw the question.

Setup is one environment variable, on the client:

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 claude
```

The chassis answers `POST /v1/messages` in the Anthropic Messages protocol, runs the
request through your `_llm` stack, then forwards it upstream and streams the response
back byte-transparent. The session behaves identically to talking to the provider
directly — while every request lands in a trace.

:::note
Authoring an `_llm` stack **is** the opt-in. A hostname without one gets a 404 on that
path; the gateway doesn't exist for it.
:::

## The stack runs before the request leaves

```txcl
WHEN @src == "llm" && @llm.phase == "request" && .request.model == "txco-test-reject"
  EMIT @llm.reject.status  = 403,
       @llm.reject.type    = "permission_error",
       @llm.reject.message = "blocked by _llm policy"
```

`.request` is the client's JSON — whatever your stack leaves there is what the upstream
receives. Four things a rule can do:

- **Reject** — `@llm.reject` renders as a native protocol error, so clients surface it
  themselves. The upstream is never contacted, so a blocked request costs nothing.
- **Rewrite** — `EMIT .request.model = "…"` pins the model whatever the client asked for.
- **Tag** — `@llm.headers.x-my-header` rides along on the upstream call.
- **Attach context** — `@llm.context`, below.

The stack runs **once per request**; the response stream never passes through it, because
the gateway owns the transport. After the exchange ends a second envelope arrives with
`@llm.phase == "completed"`, carrying status, duration, token usage, and model
provenance. No client waits on it.

:::warning
Gate every rule on `@llm.phase`. That completion envelope routes into the same stack, so
an ungated request-shaping rule fires a second time on it.
:::

## Context the model didn't have

A rule emits items at `@llm.context` — `[{source, title, content}]` — and the gateway
serializes the survivors into system blocks:

```txcl
EMIT @llm.context = [{"source": "decisions",
                      "title": "boltdb over redis",
                      "content": "..."}]
```

They are appended **after** the client's own system prompt, so its cached prefix
survives, behind a chassis-owned guard block marking them untrusted evidence rather than
instructions. Budget, item cap, and deduplication are enforced by the gateway, not your
rules: the stack decides *what*, the gateway decides *how*.

## Whose API key

Two modes, picked per tenant by what's in the
[secret store](./advanced/runbook-secret-store.md):

- **Passthrough** (nothing stored) — the client's own key is forwarded verbatim. No setup.
- **Swap** (store `ANTHROPIC_KEY` and `LLM_GATEWAY_KEY`) — clients authenticate with the
  gateway key, and the real key never leaves the server.

## What you get back

- **Model provenance is explicit** — `requested_model` (the client's ask),
  `effective_request_model` (what went upstream), and `response_model` (what was served)
  are three separate fields on the completion, never one guess.
- **Token usage is recorded, not charged.** `@llm.completion.usage.*` is presence-gated:
  absent means "not captured", never zero.
- **Injection is auditable.** `@llm.context_result` is the gateway's ground truth of what
  it injected and dropped and why — by sha256 and byte count, never the content itself,
  so [traces](./visibility.md) don't become a second copy of your notes.

Flags, the full envelope contract, and error shapes:
[the AI gateway inlet](./advanced/protocols/llm-gateway.md). A runnable walkthrough,
including retrieval against a seeded decision log: `examples/llm-gateway-hello/`.
