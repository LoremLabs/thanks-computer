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
envelope — `{{@web.req.body}}`, `{{@lmtp.msg.subject}}` —
JSON-escaped automatically so values splice into the prompt without
breaking it. Missing paths render empty rather than failing. For a
computed scratch value, use `SET PRE` (sets a field on *this op's input
only*, without propagating downstream):

```txcl
SET PRE @summary_input = .order.notes
WITH prompt = "Summarize: {{@summary_input}}"
EXEC "ai://chat"
```

For full control, skip `prompt`/`system` and author the conversation
directly: `WITH messages = [...]` (OpenAI-style `role`/`content`
turns).

## Structured output

`WITH schema = {...}` (a JSON Schema) switches to structured-output
mode: the chassis validates the model's reply against the schema and
merges the validated object as `.schema_validated_payload`. On
failure — or any provider error — the op contributes `.chat.error`
instead, and your next step's rules can resonate on that.

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

