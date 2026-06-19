# The envelope — rule-author reference

_Everything a rule can read and write on `_txc.*` (the `@` namespace),
plus the complete `WITH` directive table. Companion to the
[TXCL reference](./txcl/txcl.md); kept here so authors don't read
`processor.go` to learn the field names._

## What you read

Identity and routing (stamped by the chassis; read-only):

| Field | Meaning |
|---|---|
| `@src` | Inlet: `http`, `lmtp`, `cron`, `tcp` |
| `@rid` | Request id (trace correlation) |
| `@tenant` / `@stack` | Resolved by [ingress](../routing.md); pinned per request |
| `@ingress` / `@hostname_verified` | Matched ingress key / ownership-verification bit |
| `@op` / `@step` | The firing op's identity and scope (stamped on dispatched envelopes) |

Per-inlet request data:

| Inlet | Namespace highlights |
|---|---|
| web | `@web.req.method`, `@web.req.url.{path,hostname,port,full,query.<k>.0,query.raw}`, `@web.req.headers.<name>.0` (arrays), `@web.req.cookies.*`, `@web.req.body` (base64), `@web.req.host`, `@web.req.proto` |
| lmtp | `@lmtp.rcpt[]`, `@lmtp.msg.{subject,text,html,from[].addr,to[],headers.*,attachments[],raw}` (`text`/`html` are the parsed bodies; `raw` is the b64 original), `@lmtp.listener`; spam verdict under `@mail.spam.{score,verdict}` when an upstream Rspamd stamped it |
| cron | `@cron.job`, `@cron.tenant` |
| tcp | `@tcp.listener`, `@tcp.{local,remote}.{ip,port}` |

Stamped by builtins/AI ops: `@computed.*` (e.g. `hmac-verify`
results), `@chat.*` ([token telemetry](../ai.md)).

## What you write

Author-writable controls (via `EMIT`, or returned by an op in its
JSON):

| Field | Effect |
|---|---|
| `@halt = true` | Terminate after this scope's merge; return the document |
| `@goto = "stack/0"` or `"200"` | Jump to a stage (bare number = current stack) |
| `@ttl = N` | Lower (never raise) the remaining hop budget ([fuel.md](./fuel.md)) |
| `@web.res.status` | HTTP response status |
| `@web.res.headers.<name>` | Response headers (arrays) |
| `@web.res.body` | Response body (base64); set in a non-terminal scope it streams |
| `@lmtp.res.{code,msg,recipients[]}` | SMTP verdict back to Postfix ([lmtp.md](./protocols/lmtp.md)) |

Everything else under `_txc.*` is chassis-owned — writes to reserved
fields (`tenant`, `fuel_used`, `computed.*`, …) are rejected by the
guard. Payload fields (no `_txc.` prefix) are always yours; fields
starting with `_` are dropped from the final answer by convention.

## `WITH` — the complete directive table

| Key | Applies to | Meaning |
|---|---|---|
| `timeout` | any EXEC | Per-call wall clock (ms or `"2h"`); capped by `--op-timeout-max` |
| `method` | http(s) | HTTP verb override (default POST) |
| `secrets.headers.<h>.secret` / `.format` | http(s), builtins | Splice a stored secret into the request; `format = "Bearer {}"` templates it ([runbook](./runbook-secret-store.md)) |
| `secrets.body.<path>.secret` | http(s) | Same, into the JSON body |
| `mode = "async"` | http(s), mcp+ | Worker acks 202 now, calls back later ([continuations](../continuations.md)) |
| `mode = "continuable"` | http(s) | Answer synchronously if quick; promote to a continuation at the deadline |
| `continue_after` | continuable | The promotion deadline (default `--continue-after-default`, 5s) |
| `redact` / `omit` | any | Scrub paths from [trace](./trace.md) artifacts (runtime data untouched) |
| `debug = true` | any | Surface extra op debug detail to the trace |
| `prompt`, `system`, `messages`, `model`, `provider`, `schema`, `intent`, `limits.*` | ai://chat | The chat request — see [ai.md](../ai.md) |

## `SET` vs `SET PRE`

`SET` writes fields onto the event *before dispatch and they persist
downstream*. `SET PRE` decorates **only this op's input** — the value
never merges forward. Use it for scratch values a prompt template or
handler needs once:

```txcl
SET PRE @body_text = .ticket.description
WITH prompt = "Summarize: {{@body_text}}"
EXEC "ai://chat"
```

## Conditions: the full operator set

`==  !=  <  <=  >  >=` (numbers, lexical strings), `=~` / `!~`
(regex, `/pattern/` literals), `&&` and `||`, prefix `!`, parentheses
for grouping. A comma in `WHEN` is an AND.

## `PRIORITY` — sharper than "tie-breaker"

Default `0`. When any matching op at a scope declares a priority above
the others, **only the highest-priority op(s) run** — equal priorities
fire in parallel as usual. Use it to let a specific handler preempt a
catch-all at the same scope, without restructuring scopes.
