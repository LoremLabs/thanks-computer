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
| `txco://kv/mget` | `items`, `into?` | Read many keys — each in its own namespace, if you like — in one dispatch. |
| `txco://kv/list` | `after?`, `limit?`, `values?`, `into?` | List a namespace's keys (and values), one sorted page at a time. |

Every op also takes an optional `namespace` (see below).

## Keys are scoped per tenant + namespace

Each key is stored under **`<tenant>/<namespace>/<key>`**:

- **tenant** — the request's resolved tenant. You can't reach another tenant's keys.
- **namespace** — defaults to the **stack** serving the request, so one stack's
  keys never collide with another's. A `_`-nested inlet sub-stack
  (`<stack>/_mail`, `<stack>/_websocket`) defaults to its app stack's
  namespace (`<stack>`): the app owns its state across inlets. Pass
  `namespace = "shared"` (any name) to
  share keys across a tenant's stacks. Names starting with `_txc` are reserved
  for the chassis' own indexes (the blob name index lives in `_txc.blob`, the
  WebSocket session directory in `_txc.websocket`) and
  are refused by every `kv/*` op and by `KV/` seed packs.
- **key** — yours; no `/` (use a namespace to group).

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

## Read many keys at once — `kv/mget`

`kv/mget` reads a list of keys in **one** dispatch. `items` is an array; each
item is a bare key string or an object `{key, namespace?}`. An item without its
own `namespace` uses the call's (`WITH namespace`, else the stack's default),
so one call can read across namespaces:

```txcl
# ._cells was built by an earlier op:
#   [{"key": "triggers", "namespace": "pony-ada"},
#    {"key": "triggers", "namespace": "pony-bo"}]
WITH items = ._cells, into = "_trig"
EXEC "txco://kv/mget"
```

It writes `{items, count, found}` at `into` (default `_kv`):

```json
{
  "items": [
    {"namespace": "pony-ada", "key": "triggers", "found": true, "value": {"at": "09:00"}},
    {"namespace": "pony-bo", "key": "triggers", "found": false}
  ],
  "count": 2,
  "found": 1
}
```

- **In order.** `items[i]` answers the i-th item you asked for. A key listed
  twice is read, and reported, twice.
- **A miss is `found: false` with no `value`**, and so is an expired key.
- **At most 200 items; above that the whole call is refused.** It never
  returns a partial answer, because a dropped item would look exactly like a
  missing key. Split larger reads across calls.
- **One bad item refuses the whole call**, before anything is read: an empty
  key, a `/` in a key or namespace, or a reserved `_txc` namespace.
- `items = []` is not an error; it writes `{items: [], count: 0, found: 0}`.

Compared with N `kv/get`s, this saves N−1 dispatches (25 fuel each). On
`redis` it also turns N round trips into one `MGET` per 500 keys. On `boltdb`
the store still reads the keys one at a time, so there the saving is in
dispatch, not in the store.

## List a namespace — `kv/list`

`kv/list` returns one page of a namespace's keys, sorted: up to `limit`
(default and maximum 200) that sort after the `after` cursor.

```txcl
WITH namespace = "subscribers", limit = 100, into = "_subs"
EXEC "txco://kv/list"
#   ._subs = {"keys": ["a@x", "b@x", …], "next": "b@x", "count": 100}
```

`next` is the cursor for the following call — pass it back as `after`. It is
`""` once the namespace is exhausted.

Add `values = true` to get each key's value alongside it, as `rows`, instead of
following up with a `kv/get` per key. `keys`, `next` and `count` are still
written:

```txcl
WITH namespace = "subscribers", values = true, into = "_subs"
EXEC "txco://kv/list"
#   ._subs.rows = [{"key": "a@x", "value": {…}}, …]
```

To read a whole namespace, drain it with a `LOOP`. Arrays append across passes,
so `rows` collects every page:

```txcl
WITH namespace = "subscribers", values = true, after = ._kv.next
EXEC "txco://kv/list"
LOOP EVERY "2ms" UNTIL ._kv.next == "" MAX 50
```

There is no per-key prefix filter: the namespace *is* the prefix. Give a set
you'll want to list — subscribers, a queue — a namespace of its own.

**Listing is not cheap, and paging doesn't make it cheaper.** The store has no
cursor, so every page reads the *whole* namespace, sorts it, and returns a
window. Listing 10,000 keys 200 at a time reads the namespace 50 times. On
`redis` it is worse again: each page scans the entire keyspace, not just your
namespace. `kv/list` suits small namespaces and occasional sweeps; don't list a
large namespace on every request. `values = true` adds no store work — the
values are read either way.

## Notes

- KV ops pay normal [fuel](./fuel.md) and appear in [traces](./trace.md).
  `kv/mget`, and `kv/list` with `values = true`, also pay 100 fuel per MiB of
  values returned, rounded up — so any call that returns a value pays at least
  100.
- Values over `--kv-max-value-bytes` (default 64 KiB) are rejected.
- With `boltdb` each chassis keeps its own store; switch to `redis` when several
  chassis must share state.
