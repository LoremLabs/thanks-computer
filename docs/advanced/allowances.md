# Allowances — a tenant's own fuel budgets

_Reference for `txco://allowance/*` and `txco allowance`: how a tenant
gives part of its work a fuel budget of its own, and what happens when that
budget is spent._

A tenant pays for its [fuel](./fuel.md) as one budget. An **allowance** is a
budget the tenant carves out of it: so much fuel per UTC hour, day or month,
for whatever unit the tenant cares about — a customer, an agent, a team, a
feature. A stack puts a request in an allowance; from then on that request's
fuel counts against the allowance **and** the tenant. When the allowance's
window is spent, its requests are refused until the window resets, while the
rest of the tenant keeps running. When the tenant's own budget is spent,
everything stops, allowance or not: whichever runs out first refuses.

The tenant defines and manages its allowances; the chassis only enforces
them.

## Defining one

An allowance is `{name, fuel, per}`:

| Field | Meaning |
| ----- | ------- |
| `name` | 1–64 of `a-z 0-9 . _ -`, starting with a letter or digit. The tenant's own label. |
| `fuel` | The limit per window, in fuel (positive). |
| `per`  | `hour`, `day` (default) or `month` — fixed UTC windows. |

From a stack:

```txcl
EXEC "txco://allowance/set"
  WITH name = "pony-scout", fuel = 2500000, per = "day"
```

Or from the CLI, against the tenant your profile signs for:

```
txco allowance set pony-scout --fuel 2500000 --per day
txco allowance list
txco allowance get pony-scout
txco allowance delete pony-scout
```

Setting an allowance again replaces it and keeps its counters, so raising
the limit mid-window frees what is left of the window at once. Deleting one
removes the definition; its counters expire with their windows.

## Entering one

```txcl
# its own scope, before the work it pays for
WHEN @src == "http" && @web.req.url.path == "/work"
  EXEC "txco://allowance/enter"
    WITH name = ._req.customer,
         into = "_al"
```

- **Write-once.** A request enters at most one allowance. Entering another
  is an op error; entering the same one again just re-checks it. Nothing
  downstream — a later rule, a goto into another stack, a continuation —
  can move a request out of the allowance it entered.
- **The refusal comes at the next scope entry.** If the window is spent,
  `enter` answers `{denied: true}` and the request ends at its next budget
  check: the next scope entry, or the next pass of a `LOOP`. Rules in the
  same scope as `enter` still run, so give `enter` a scope of its own,
  before the work.
- **A ceiling for the request.** If the window has fuel left, the request's
  fuel ceiling drops to what it had already burned plus what the window has
  left (never above `--max-fuel-per-request`). One runaway request therefore
  stops near the allowance's limit instead of at the chassis-wide cap, and
  it is refused the same way.
- **No definition, no limit.** Entering a name that was never `set` is
  metered — `used` counts, by day — but never refused. A tenant can measure
  first and set limits later.
- **An outage admits.** If the KV store can't be read, the request still
  enters, metered but unchecked (`checked: false`). The tenant's own budget
  still applies; an allowance outage does not take the tenant down with it.

`enter` writes the window's state at `into` (default `_allowance`):

```json
{"name":"pony-scout","defined":true,"per":"day","used":1830000,"fuel":2500000,
 "remaining":670000,"resets_at":"2026-10-08T00:00:00Z",
 "entered":true,"denied":false,"checked":true}
```

The request carries the allowance as `@allowance` (`_txc.allowance`:
`name`, `fuel`, `resets_at`, and `cap`, the ceiling it set). Rules can read
it; no rule can write it.

`txco://allowance/get` reads the same object without entering
(`WITH name`, or none for the allowance the request already entered). Use it
to degrade gracefully before entering — e.g. answer "I've used today's
budget" instead of being refused. `txco://allowance/list` pages through the
definitions (`after`, `limit`).

## The refusal

A spent allowance is refused through the same admission marker as a tenant
over its rate limit, so every head renders it in its own protocol:

| Head | Answer |
| ---- | ------ |
| web | `429 Too Many Requests`, `Retry-After: <seconds to the window's end>`, `x-txc-deny-reason: allowance_exhausted` |
| LMTP | `451` temporary failure — the sender's server retries the mail after the window |
| others | the head's own rendering of an admission denial |

The run's final payload carries the error:

```json
{"code":"txco_allowance_exhausted","allowance":"pony-scout","fuel":2500000,
 "resets_at":"2026-10-08T00:00:00Z","fuel_used":145,"last_stage":"core/0110"}
```

Unlike other admission denials, this one comes after the stack ran: the
fuel the request burned before entering is real usage and is billed, to the
tenant and the allowance.

## How it is counted

- Fuel is charged from the [usage](./usage.md) path: the request's own
  line, resumed continuation segments, and the heartbeats of an attachment
  the request opened. Every billable usage event under an allowance adds
  its fuel to that allowance's current window. With `--usage-enabled=false`
  nothing is charged.
- Charges are summed in memory and written to the KV once a second per
  allowance, so the counter trails the requests that just finished by about
  a second.
- **Overshoot.** A request checks the window when it enters and is charged
  when it ends. Requests already running when the window crosses its limit
  finish (each within its own ceiling), so a busy allowance can end a window
  somewhat over its fuel. The next request after that is refused.
- Allowances and their counters live in the tenant's KV, under chassis-
  reserved namespaces (`_txc.allowance`, `_txc.allowance.used`). On a fleet,
  point `--kvstore` at shared storage (redis) so every node counts against
  the same windows. `txco://kv/*` cannot read or write them.

## Permissions

`txco allowance` uses the signed admin API
(`/v1/tenants/{tenant}/allowances[/{name}]`): `kv:*:read` to list and get,
`kv:*:write` to set and delete. Tenant owners have both.

The `txco://allowance/*` ops are open to every rule in the tenant's stacks:
the tenant's rules are the tenant, and they only ever see their own
tenant's allowances.
