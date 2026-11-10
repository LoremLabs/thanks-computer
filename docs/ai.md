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

With `provider = "openai"`, a schema whose top level is an object also
turns on OpenAI's JSON mode, so the reply is always well-formed JSON
(the schema itself is still checked by the chassis). OpenAI allows JSON
mode only when the messages mention JSON, so say so in the system text
("Respond with JSON only"); without it the call goes out as before.

Add `WITH strict = "prefer"` and, on OpenAI, a schema that qualifies
([strict decoding](#tools--the-model-calls-your-capabilities)) is sent as
`response_format: json_schema` with `strict: true`: the reply can only
match the schema. `_txc.chat.schema_mode` then says which went out,
`json_schema` or `json_object`. The chassis validates the reply either
way.

## Tools — the model calls your capabilities

A model can be offered **tools**: named operations with a JSON Schema for
their arguments, which it asks for by name instead of describing what it
wants in prose. `ai://chat` sends the definitions in the provider's own
tool-calling form and hands back the calls the model made. It never
*runs* a call: your stack does, with `cap://`, under the same gateway as
any other call, and sends the result back on the next request. The loop
stays in txcl, where every call is decided, traced and able to end a run.

```txcl
# 1. offer two capabilities as tools (their definitions come from the catalogue)
WHEN ._ask.text =~ /./
  WITH names = ["stock.check", "price.quote"], format = "tools", into = "_tools"
  EXEC "txco://caps/list"

# 2. ask the model
WHEN ._tools.count > 0
  WITH prompt = "{{@_ask.text}}",
       tools  = ._tools.tools,
       into   = "_turn"
  EXEC "ai://chat"

# 3. run the call it made, through the capability its binding names
WHEN ._turn.tool_calls.0.name =~ /./ && !exists ._turn.tool_calls.0.problem
  WITH name  = ._turn.tool_calls.0.binding.capability,
       input = ._turn.tool_calls.0.arguments,
       into  = "_res"
  EXEC "cap://tool"

# 4. give the result back and get the answer
WHEN ._res.in_stock != null
  WITH messages = &array(&object("role", "user", "content", ._ask.text),
                         ._turn.message,
                         &object("role", "tool", "tool_call_id", ._turn.tool_calls.0.id, "content", ._res)),
       tools    = ._tools.tools,
       into     = "_final"
  EXEC "ai://chat"
```

**A tool is a definition, not a name.** `WITH tools` takes an array of
`{name, description, parameters, binding?}`: `parameters` is an object
JSON Schema, `name` is letters, digits, `_` and `-` (at most 64: what
every provider accepts), and `binding` is yours — the chassis never reads
it and never sends it, it copies it onto each call to that tool so one
dispatch rule needs no lookup. A string where a definition belongs is an
error: `ai://chat` resolves nothing against the catalogue. The canonical
source of definitions is `txco://caps/list WITH format = "tools"`, which
derives one from each capability's declaration with
`binding = {capability: <its name>}` ([capabilities](./advanced/capabilities.md#the-catalogue));
an executor writes its own for tools it found elsewhere (an MCP server's,
an OpenAPI operation), binding them to the capability that runs them.

**What comes back** (at the top level, or under `WITH into`):

| field | |
|---|---|
| `text` | The model's text; empty when the answer is calls alone |
| `finish` | `stop`, `tool_calls`, `length`, `filtered` or `other`; absent when the provider did not say |
| `tool_calls[]` | Each `{id, name, arguments, binding}`. `arguments` is the JSON object the model wrote, or `null` with `raw` holding its text when it was not one. `binding` is the offered tool's, never the provider's |
| `tool_calls[].problem` | Set when a call must not run as it is: `not_offered` (a name this request did not offer), `not_json`, `truncated` (the answer hit the output limit, so every call's arguments may be cut off), `duplicate_id`. Answer such a call with an error `tool` message and the model corrects itself |
| `message` | The assistant turn as a message, ready to append to the next request's `messages` |
| `response_id` | The provider's id for the response, when it gave one |

`chat.error` and the `_txc.chat.*` stamps stay at the top level whatever
`into` says, so `WHEN @chat.error` works the same on every call. Several
calls in one answer are normal: run them in order, or side by side in one
scope with a [`cap_into`](./advanced/capabilities.md#calls-side-by-side)
each, and send every result before asking again.

**The chassis checks shape, not meaning.** It refuses a call to a tool
that was not offered and marks arguments that are not an object, but it
does not check arguments against the tool's schema: your rule does, with
`txco://schema/check`, before the `cap://` — the executor knows where a
schema came from, and the gateway is the authorization boundary either way.
A call may run only if it was offered on that turn *and* is permitted for
that run, wherever its capability is answered.

**A transcript must be complete.** Every call in an assistant turn needs
its `tool` message (`{role: "tool", tool_call_id, content}`, `content`
any JSON) before the next turn, in any order. One that is missing is not
a mistake the chassis repairs: the request is refused with
`chat.error.code = "txco_chat_pending_tool_result"` and the unanswered ids
in `chat.error.detail.ids`, and your rule decides — write the result,
wait for an approval, settle an uncertain effect. `WITH repair =
"error_results"` answers every unanswered call with a synthetic error
result instead; it is for importing a historical transcript, never for
resuming a live run. A malformed transcript (a result for a call that was
never made, an id repeated within one turn) is the author's bug and the
op is dropped, as for any bad `WITH`.

| `WITH` | |
|---|---|
| `tools` | The definitions, as above. Allowed with `prompt` or `messages` |
| `tool_choice` | `auto` (default), `none`, `required`, or one offered tool's name to force it |
| `parallel` | `false` asks the model for at most one call per answer; absent leaves it to the model |
| `repair` | `error_results`: see above |
| `strict` | `off` (default), `prefer` or `require`: strict decoding, below |
| `into` | Where the answer lands; the default is the top level. Not under `_txc` and not `chat` |

**Strict decoding.** With `strict = "prefer"` a provider that can (OpenAI)
decodes a tool's arguments with a grammar built from its schema, so they
can only match it; a tool whose schema cannot be decoded that way is sent
as it is. `require` refuses the request instead (`txco_chat_strict_unsupported`),
so a stack that depends on it never silently gets less. A schema qualifies
when every field is typed, no object is open to unknown keys, no optional
field allows null (its absence could not be told from a null), and it uses
no `$ref`; the typed declarations the catalogue derives qualify by
construction once their fields have types. The chassis sends an optional
field as required-and-nullable and removes the null the model returns for
it, so the executor sees the field absent, as the declaration meant; a null
at a field the declaration itself allows stays. Strict decoding is a
decoding aid: validate the arguments with `txco://schema/check` either way.

| `chat.error.code` | |
|---|---|
| `txco_chat_tools_unsupported` | The backend takes no tools, or cannot bound parallel calls when asked to. Refused before any secret is read or anything is sent |
| `txco_chat_pending_tool_result` | The transcript has unanswered calls (`detail.ids`) |
| `txco_chat_tool_invalid`, `txco_chat_tool_names` | A malformed definition or transcript, a choice naming no offered tool, `parallel` without `tools`, two definitions with one name. The author's; the op is dropped |

The step's model facts record `tools_offered`, the `tool_calls` names and
`finish`, never the arguments. Tool definitions are input tokens like any
other: counted, cached by the provider when the prefix holds, never fuel.

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
  `_txc.chat.{provider, model, tokens.{in, out, cached, reasoning},
  latency_ms, retries}` — visible in the [trace](./visibility.md),
  aggregatable for cost reporting. Token counts are *not* charged to
  [fuel](./advanced/fuel.md): provider compute is its own dimension.
- **Time per call.** The op's `WITH timeout` bounds a call; without one,
  the chassis-wide default for AI ops is `--ai-default-timeout` (60s —
  deliberately longer than the 5s op default). `WITH limits.timeout_ms`
  and `limits.max_cost_usd` are accepted but enforced by no backend
  today: use `timeout`, and do not count on a cost ceiling.

## The other direction

Everything above is *outbound*: your stack calls a model. The chassis also runs an
**inbound** [AI gateway](./gateway.md) — point an existing AI client at it and the
requests *it* sends become operations, which a stack can reject, rewrite, or answer with
context the model didn't have. Different direction, different keys: the gateway reads the
tenant secret `ANTHROPIC_KEY`, never an environment variable.

