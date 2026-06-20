# Resonators

A resonator dictates when an operation runs in [Thanks, Computer](https://www.thanks.computer).

## Sequencing by steps

You may be familiar with the `BASIC` programming language that had line numbers.
The rule attached to each [operation](./ops.md) is called a **resonator**: a filter that decides whether the operation fires at all. 
With TXCO, each operation's resonator has a `STEP` that it resonates at. 

```stack
steps 
0 hello
10 world
```

As [requests come into a stack](./routing.md), they start at the top of the stack, at `STEP 0`. The
`txco chassis` first gathers all operations at that step, and then evaluates their 
resonators to see if we should execute the operation. [If there is no step 0, txco
automatically moves on to the next highest step.]

If there are multiple [ops](ops.md) at a step, they are evaluated in parallel:

```stack
steps 
10 hello world
```
Advanced stacks use this parallel processing to pre-load data that you know you will need later at a step
that you know takes a long time to process. But for now let's look at the simplest case, a single rule that always runs.

## The smallest rule called a resonator is one line:

```txcl
EMIT .hello = "world"
```

In this sample operation, the flow memory gets `{"hello": "world"}` merged in
and available for future operations.

Implicit in this rule is:

```txcl
WHEN *
EMIT .hello = "world"
```

Which is what makes it always resonate and contribute: 

```json
{
    hello: "world"
}
```

## The resonator — execute only when needed

A stack may hold dozens of operations, but for any given event most of
them may stay silent. Like a tuning fork, it's cut for a particular kind of event and
only rings when one matches — everything else passes by untouched.

The `WHEN` clause is the resonance condition:

```txcl
WHEN @web.req.url.path == "/invoice"   # fire for one path
WHEN .amount > 1000                    # fire when the payload says so
WHEN .tier == "vip"                    # fire on what a previous step found
```

Every operation's output merges into the shared event, so a condition on `.tier` is
really a condition on *what an earlier step concluded*. The classifier
doesn't call the VIP handler — it just emits `.tier = "vip"`, and at
the next step the VIP handler's resonator picks it up. 

That's the shape it creates — handlers sitting in parallel at a step,
the previous step's conclusion deciding which one rings:

```stack
orders
100 classify
200 vip standard
```

A condition plus a dispatch is a working integration:

```txcl
WHEN .tz == "ams"
EXEC "https://timeapi.io/api/v1/time/current/zone?timezone=Europe%2FAmsterdam"
```

Only events with `.tz == "ams"` reach the URL; the HTTP response merges
back into the event.

## Describe in English

A resonator has up to seven English clauses — all optional, evaluated in this
order:

| Clause     | What it does                                                    |
| ---------- | --------------------------------------------------------------- |
| `WHEN`     | The resonance condition (omitted or `*` = always fire)          |
| `SET`      | Set fields on the event *before* dispatch                       |
| `SELECT`   | Project what the operation receives (pass a tree).              |
| `WITH`     | Per-call directives — `timeout`, `secrets.*`, `redact`, …       |
| `PRIORITY` | Tie-breaker among matches at the same step                      |
| `EXEC`     | Dispatch target — `op://`, `http(s)://`, `txco://`, `mcp+https://` |
| `EMIT`     | Overlay values onto the response, *after* dispatch              |

A complete rule using the most common:

```txcl
WHEN .user.id =~ /^u_/
SET .source = "thanks-computer"
WITH timeout = 2000
EXEC "https://api.example.com/enrich"
EMIT .enriched_at = &now("rfc3339")
```

## Working with Data: Dot-Path Access

Paths are dot-paths into the event JSON. `.user.id` reads the payload;
`@` is shorthand for `._txc.` — the chassis's envelope — so
`@web.req.url.path` is the request path, `@halt = true` stops the flow,
and `@goto = "billing/0"` jumps to another stack.

## Values can compute

Values are literals or function calls: `&uuid()`, `&now("rfc3339")`,
`&json(...)`, `&b64decode(...)` — inline, pure computation without
dispatching anything.

## Private via the `_` convention

The last step's context becomes the output. Output hides any part of the 
context document that starts with an `_` by convention. Thus, `_shh` is private
and `hi` is returned in the answer.

:::note
`_`-prefixed fields are dropped from the **response**, but they're still visible
to **downstream operations** in the same flow. To keep a value out of a later op's
input too, project with `SELECT`.
:::

## That's most of it

Keywords are case-insensitive, `#` starts a comment, whitespace is
free-form — a rule on one line and the same rule on five are identical.
See [Operations](./ops.md) for what `EXEC` can point at.
