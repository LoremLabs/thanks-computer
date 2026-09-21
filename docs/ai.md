# AI Operations — Using `EXEC ai://chat`

_In [Thanks, Computer](https://www.thanks.computer), an AI model is just another [operation](./ops.md):
gated by a resonator, reading the shared document, merging its answer back._

One rule puts a model in the flow:

```txcl
WHEN @src == "lmtp"
WITH prompt  = "Classify this support mail: {{@lmtp.msg.text}}",
     system  = "You answer with one word: billing, technical, or other.",
     model   = "openai/gpt-4o-mini",
     intent  = "classify_support_ticket"
EXEC "ai://chat"
```

The model's reply merges into the document as `.text` — and whatever
resonates with that at the next step, fires. The classifier doesn't
route; it just answers, like every other op.

:::note
`ai://chat` needs an OpenRouter API key, stored as the secret `OPENROUTER_KEY`.
Set it before your first call — see [Set the API key](#set-the-api-key) below.
:::

## Prompts read the document

`{{@path}}` markers in `prompt` and `system` are filled from the
op's input — `{{@web.req.body}}`, `{{@lmtp.msg.subject}}` —
JSON-escaped automatically so values splice into the prompt without
breaking it. Missing paths render empty rather than failing. On a rule
with a `SELECT` clause, the input is the projected view, so select the
paths the prompt references. For a computed scratch value, use a plain
`SET` (before any `SELECT`, a `SET` decorates *this op's input only*,
without propagating downstream):

```txcl
SET @summary_input = .order.notes
WITH prompt = "Summarize: {{@summary_input}}"
EXEC "ai://chat"
```

For full control, skip `prompt`/`system` and author the conversation
directly: `WITH messages = [...]` (OpenAI-style `role`/`content`
turns).

A long system prompt can live in its own file beside the op:
`WITH system = &include("system.md")`
([Including files](./advanced/txcl/txcl.md#including-files--include)).
It is still a template: its `{{@path}}` markers are filled like any
other, and any other `{{…}}` in it (a Handlebars example, say) is an
error, so keep such text out of included prompts.

## Structured output

`WITH schema = {...}` (a JSON Schema) switches to structured-output
mode: the chassis validates the model's reply against the schema and
merges the validated object as `.schema_validated_payload`. On
failure — or any provider error — the op contributes `.chat.error`
instead, and your next step's rules can resonate on that.

## Embeddings — `EXEC "ai://embed"`

Where `ai://chat` returns prose, `ai://embed` returns a **vector** — the numeric
fingerprint of a piece of text, for semantic search. It's the companion to the
[vector store](./vectors.md): embed the query, then search.

```txcl
WITH provider = "openai",
     model    = "text-embedding-3-small",
     text     = "a cozy book for a rainy afternoon"
EXEC "ai://embed"
```

The vector merges in under `_embed.vector` (a float array), alongside
`_embed.{model, dimensions, tokens}`. Embed many strings in one call with
`WITH texts = [ … ]` → `_embed.vectors` (one per input). On any provider error
the op contributes `_embed.error` instead, and your next step can resonate on it.

Two backends ship in the box:

- **`provider = "ollama"`** (the dev default) — a local [Ollama](https://ollama.com)
  running `nomic-embed-text`, no API key. Point it with `--embed-ollama-base-url`.
- **`provider = "openai"`** — OpenAI's embedding models
  (`text-embedding-3-small` / `-large`); needs the per-tenant secret `OPENAI_KEY`
  (stored the same way as `OPENROUTER_KEY` below). `WITH dimensions = N` requests a
  shortened vector where the model supports it.

Pick one embedding model per [collection](./vectors.md#collections) and stay on it
— vectors are only comparable within the same embedding space, so a collection
pins its model and rejects a mismatched upsert.

## Decisions — `EXEC "ai://decide"`

Some questions don't need prose back. *Which folder does this belong in? Was
this sent by a machine? How urgent is it?* `ai://decide` asks them directly:
you give it the evidence (`state`) and a set of bounded questions, and each
answer comes back typed, with probabilities. The model can only pick from the
answers you offered.

```txcl
WHEN ._doc.ready == true
WITH state     = ._doc.summary,
     questions = &object(
       "folder",    &object("type", "choice",
                            "instructions", "Which folder does this document belong in?",
                            "criteria", &object("Invoices",  "bills and receipts",
                                                "Contracts", "signed agreements")),
       "automated", &object("type", "noul",
                            "instructions", "Was this sent by a machine rather than a person?")),
     into      = "_place",
     timeout   = 5000
EXEC "ai://decide"
```

`state` is data: a string, an object, or an array (never a template). There
are three kinds of question:

| `type` | Asks | `criteria` | Answer |
| --- | --- | --- | --- |
| `noul` | is this proposition true? | optional: `{"true": "…", "false": "…"}` | `probability` = P(true) |
| `choice` | which of these fits? | required: option name → description | `choice`, `probability` = P(choice), `probabilities` per option |
| `score` | how far along this rubric? | required: levels, lowest first (≥ 2) | `score` (0 = lowest level), `probabilities` per level index |

When the provider reports its own `confidence` in an answer, that rides along
too (Jev reports one for `choice` and `score`, not `noul`). It is the
provider's number, not a probability, and it is never made up when absent.

The result lands under `WITH into` (default `_decide`):

```json
{
  "ok": true,
  "answers": {
    "folder":    { "type": "choice", "choice": "Invoices", "probability": 0.91,
                   "confidence": 0.93,
                   "probabilities": { "Invoices": 0.91, "Contracts": 0.09 } },
    "automated": { "type": "noul", "probability": 0.04 }
  },
  "provider": "vercel", "model": "typesafe-ai/jev",
  "usage": { "input_tokens": 275, "output_tokens": 20 }, "latency_ms": 180
}
```

### Your stack decides what a probability means

The op has no threshold, fallback or allow/deny setting. It returns
judgments; the next step's WHEN clauses turn them into policy, where they are
visible and diffable:

```txcl
WHEN ._place.ok == true && ._place.answers.folder.choice == "Invoices"
     && ._place.answers.folder.probability > 0.80
EMIT .folder = "Invoices"
```

```txcl
WHEN ._place.ok != true
EMIT .folder = "Inbox"
```

The failure lane is yours too: fall back, fail closed, ask a human. The op
never turns a failure into an answer. Two WHEN rules matter for every
threshold:

- **Gate on `ok == true` first.** A missing path compares as 0. When the call
  fails there are no answers, so `probability > 0.8` stays quiet but
  `probability < 0.2` **fires**.
- **Write thresholds as decimals.** An integer literal compares as an
  integer: `probability > 0` is false for 0.91. Use `> 0.0`.

On failure, `ok` is `false`, `answers` is absent, and `error.code` says why:

| `error.code` | Meaning |
| --- | --- |
| `txco_decide_invalid_with` | malformed `state`/`questions`/`into`, or a question beyond the provider's limits. Nothing was sent. |
| `txco_decide_missing_secret` | no `VERCEL_AI_KEY` for this tenant |
| `txco_decide_timeout` | the op's timeout passed first |
| `txco_decide_provider_http` / `_provider_net` / `_provider_parse` | the provider refused, was unreachable, or answered garbage |
| `txco_decide_invalid_answer` | the provider's answers broke the contract (a question unanswered, a choice not offered, a probability out of range) |
| `txco_decide_no_backend` | `WITH provider` names no registered backend |

Each question key comes back under the same key, so keys are limited to
letters, digits, `_` and `-`. The one backend today, **`provider = "vercel"`**
(the default), sends the questions to Vercel AI Gateway's evaluation API. It
runs TypeSafe AI's Jev (`typesafe-ai/jev`) by default and accepts up to 255
options per choice. It needs the per-tenant secret `VERCEL_AI_KEY`,
stored the same way as `OPENROUTER_KEY` below. Decision state is often private
(mail, documents), so the chassis asks the provider not to retain it
(`--decide-zero-data-retention`, on by default). Jev honors that; a model
without a zero-retention agreement refuses the call rather than serving it.
Decisions take well under a second (about 300 ms warm), so set a tight
`WITH timeout`; the AI default is 60s.

To test a flow without calling a provider, mock the op with the result shape
above (what the op merges), not the provider's wire format.

A worked example lives at `examples/decide-produce/`: type a food, and four
questions plus four `WHEN` rules answer "vegetable", "not sure", or "not
produce".

## Set the API key

`ai://chat` routes through [OpenRouter](https://openrouter.ai), so the chassis
needs an OpenRouter API key. Store it as a per-tenant secret named
`OPENROUTER_KEY` with the [secret-store](./advanced/runbook-secret-store.md) CLI —
you paste the key at a hidden prompt, so it never touches your shell history:

```sh
txco auth tenant secrets set OPENROUTER_KEY
# add --tenant <slug> to target a specific tenant
```

Grab a key from [openrouter.ai/keys](https://openrouter.ai/keys). The chassis
materializes it into the op at call time; the cleartext never reaches traces,
logs, or continuations.

On a dev machine you can skip the secret store and export the key instead — the
chassis falls back to a same-named environment variable
(`--ai-chat-env-fallback`, default on):

```sh
export OPENROUTER_KEY=sk-or-...
```

Set `--ai-chat-env-fallback=false` on shared deployments so each tenant must
provision its own key.

## Keys, costs, and accounting

- **Secrets come from the [secret store](./advanced/runbook-secret-store.md).** The
  backend declares what it needs (the OpenRouter backend: `OPENROUTER_KEY`) and the
  chassis materializes it per tenant. Cleartext is contained by construction — it
  can't reach traces, logs, or continuations.
- **Telemetry rides the envelope.** Every call stamps
  `_txc.chat.{provider, model, tokens.in, tokens.out, latency_ms,
  retries}` — visible in the [trace](./visibility.md), aggregatable for
  cost reporting. Token counts are *not* charged to
  [fuel](./advanced/fuel.md): provider compute is its own dimension.
- **Limits per call.** `WITH limits.timeout_ms` and
  `limits.max_cost_usd` cap one call; the chassis-wide default timeout
  for AI ops is `--ai-default-timeout` (60s — deliberately longer than
  the 5s op default).

## The other direction

Everything above is *outbound*: your stack calls a model. The chassis also runs an
**inbound** [AI gateway](./gateway.md) — point an existing AI client at it and the
requests *it* sends become operations, which a stack can reject, rewrite, or answer with
context the model didn't have. Different direction, different keys: the gateway reads the
tenant secret `ANTHROPIC_KEY`, never an environment variable.

