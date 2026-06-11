# TXCL — the rule language

_In Thanks, Computer, every [operation](./ops.md) is gated by a rule
written in TXCL — a few readable lines saying when it resonates and what it
contributes to the flow. ([Overview](./overview.md))_

The smallest rule is one line:

```txcl
EMIT .hello = "world"
```

Every event gets `{"hello": "world"}` merged in. Real rules almost
always start with a condition — and that's the idea to get first.

## The resonator — fire only when needed

A stack may hold dozens of operations, but for any given event most of
them should stay silent. The rule attached to each operation is called
a **resonator**: a filter that decides whether the operation fires at
all. Like a tuning fork, it's cut for a particular kind of event and
only rings when one matches — everything else passes by untouched.

The `WHEN` clause is the resonance condition:

```txcl
WHEN @web.req.url.path == "/invoice"   # fire for one path
WHEN .amount > 1000                    # fire when the payload says so
WHEN .tier == "vip"                    # fire on what a previous step found
```

That last one is the trick that makes flows compose. Every operation's
output merges into the shared event, so a condition on `.tier` is
really a condition on *what an earlier step concluded*. The classifier
doesn't call the VIP handler — it just emits `.tier = "vip"`, and at
the next step the VIP handler's resonator picks it up. Steps chain by
resonance, not by wiring.

A condition plus a dispatch is a working integration:

```txcl
WHEN .tz == "ams"
EXEC "https://timeapi.io/api/v1/time/current/zone?timezone=Europe%2FAmsterdam"
```

Only events with `.tz == "ams"` reach the URL; the HTTP response merges
back into the event.

## The seven clauses

A resonator has up to seven clauses — all optional, evaluated in this
order:

| Clause     | What it does                                                    |
| ---------- | --------------------------------------------------------------- |
| `WHEN`     | The resonance condition (omitted or `*` = always fire)          |
| `SET`      | Set fields on the event *before* dispatch                       |
| `SELECT`   | Project what the operation receives                             |
| `WITH`     | Per-call directives — `timeout`, `secrets.*`, `redact`, …       |
| `PRIORITY` | Tie-breaker among matches at the same step                      |
| `EXEC`     | Dispatch target — `op://`, `http(s)://`, `txco://`, `mcp+https://` |
| `EMIT`     | Overlay values onto the response, *after* dispatch              |

A complete rule using most of them:

```txcl
WHEN .user.id =~ /^u_/
SET .source = "thanks-computer"
WITH timeout = 2000
EXEC "https://api.example.com/enrich"
EMIT .enriched_at = &now("rfc3339")
```

## Reading paths

Paths are dot-paths into the event JSON. `.user.id` reads the payload;
`@` is shorthand for `._txc.` — the chassis's envelope — so
`@web.req.url.path` is the request path, `@halt = true` stops the flow,
and `@goto = "billing/0"` jumps to another stack.

## Values can compute

Values are literals or function calls: `&uuid()`, `&now("rfc3339")`,
`&json(...)`, `&b64decode(...)` — inline, pure computation without
dispatching anything.

## Private by convention

The last step's context becomes the output. Output hides any part of the 
context document that starts with an `_` by convention. Thus, `_shh` is private
and `hi` is returned in the answer.

## That's most of it

Keywords are case-insensitive, `#` starts a comment, whitespace is
free-form — a rule on one line and the same rule on five are identical.
See [Operations](./ops.md) for what `EXEC` can point at.
