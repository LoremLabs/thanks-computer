<!-- nav: KV store -->

# KV store — persist values across requests

_`txco://kv/*` is the one place an op can both read AND write durable state. The
envelope lives for a single request; the KV store outlives it, so rules can keep
counters, flags, locks, cached lookups — any small JSON — between requests._

Pick a backend with `--kvstore`:

- **`boltdb`** (default) — an embedded on-disk store, local to that one chassis.
  Zero setup; right for a single chassis or dev.
- **`redis`** — a shared redis that several chassis point at (`--kvstore-addrs`),
  so they all read and write the same keys. Use it when more than one chassis
  serves the same tenants and they need to see each other's writes.

The ops are identical either way.

## The ops

| Op | WITH | Does |
|---|---|---|
| `txco://kv/get` | `key`, `into?`, `fallback?` | Read a value into the envelope. |
| `txco://kv/set` | `key`, `value?` / `from?`, `ttl?` | Write a value. |
| `txco://kv/delete` | `key` | Remove a value. |
| `txco://kv/incr` | `key`, `by?`, `ttl?`, `into?` | Atomically add to an integer. |
| `txco://kv/cas` | `key`, `value?` / `from?`, `expected?`, `ttl?`, `into?` | Check-and-set. |

Every op also takes an optional `namespace` (see below).

## Keys are scoped per tenant + namespace

Each key is stored under **`<tenant>/<namespace>/<key>`**:

- **tenant** — the request's resolved tenant. You can't reach another tenant's keys.
- **namespace** — defaults to the **stack** serving the request, so one stack's
  keys never collide with another's. Pass `namespace = "shared"` (any name) to
  share keys across a tenant's stacks.
- **key** — yours; no `.` or `/` (use a namespace to group).

So `kv/incr key="hits"` from stack `web` of tenant `acme` touches `acme/web/hits`.

## Values are JSON; results land at `into`

Values are arbitrary JSON. `kv/get` / `kv/incr` / `kv/cas` write their result into
the envelope at `into` (default `_kv`). `_kv` is `_`-prefixed, so it's dropped
from the default web response — a scratch slot the client never sees. Point
`into` at a non-private path to surface a value, e.g. `into = ".count"`.

```txcl
# read a counter into the response, defaulting to 0 when unset
WITH key = "hits", into = ".hits", fallback = 0
EXEC "txco://kv/get"
```

> The fallback param is `fallback`, not `default` — `default` is a reserved txcl keyword.

## set / delete

```txcl
# a literal value
WITH key = "greeting", value = "hello"
EXEC "txco://kv/set"

# a value pulled from an envelope path
WITH key = "ua", from = "@web.req.headers.user-agent.0"
EXEC "txco://kv/set"

WITH key = "greeting"
EXEC "txco://kv/delete"
```

## TTL — values can expire (opt-in)

`kv/set` and `kv/incr` take an optional `ttl` in **seconds**. Omit it (the
default) and the key is **persistent** — it lives until you overwrite or delete
it. With a `ttl`, the key vanishes once it lapses.

```txcl
# a 10-minute cache entry
WITH key = "rates", from = ".fetched", ttl = 600
EXEC "txco://kv/set"
```

An operator can cap the maximum with `--kv-max-ttl` (a larger requested `ttl`
clamps down to it).

## Atomic counters — `kv/incr`

`kv/incr` adds `by` (default `1`) to an integer key and writes the new value to
`into`. It's atomic — concurrent requests never lose an update, even across
chassis sharing one redis. `by` is signed, so a **negative `by` decrements**.

```txcl
WITH key = "page:hits", by = 1, into = ".count"
EXEC "txco://kv/incr"

WITH key = "inventory", by = -1, into = ".left"
EXEC "txco://kv/incr"
```

## Check-and-set — `kv/cas`

`kv/cas` writes a new value **only if** the current value equals `expected` —
or, with `expected` omitted, **only if the key is absent**. It reports
`{swapped, current}` at `into`: `swapped` is whether it wrote, and `current` is
the value now in the store. On a failed check `current` is the *real* current,
so you can recompute and retry.

```txcl
# optimistic update — write only if nobody changed it since you read it
WITH key = "config", expected = .prev, value = .next
EXEC "txco://kv/cas"
#   ._kv.swapped == false → ._kv.current holds the latest; retry against it
```

With `expected` omitted it's a **lock** — only the first caller wins:

```txcl
# scope 100 — try to take the lock
WITH key = "job:42:lock", value = "me"
EXEC "txco://kv/cas"

# scope 200 — proceed only if we got it (later scope: same-scope ops run in parallel)
WHEN ._kv.swapped == true
EMIT .status = "running"
```

That's also how you build a safe state machine: `kv/get` to read, decide in a
rule, then `kv/cas` with the value you read as `expected` — the write only lands
if the world hasn't moved under you.

## Notes

- KV ops pay normal [fuel](./fuel.md) and appear in [traces](./trace.md).
- Values over `--kv-max-value-bytes` (default 64 KiB) are rejected.
- With `boltdb` each chassis keeps its own store; switch to `redis` when several
  chassis must share state.
