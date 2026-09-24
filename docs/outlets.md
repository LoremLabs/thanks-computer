# Outlets

A stack can read and write the database its owner already runs — the CRM,
the ticket table, the orders schema — through an **outlet**: a named
connection the stack declares once, with the credential held by the
chassis. The op writes its own SQL with every value bound as a parameter;
the chassis owns the pool, bounds rows, bytes and time, and hands the op
JSON. The stack never sees a DSN, a password or a socket.

An inlet lets an external system speak into a stack; an outlet lets a stack
speak out to one. PostgreSQL is the first driver.

## Authoring

One file per outlet under the reserved `OUTLETS/` subtree:

```
OPS/<stack>/
  OUTLETS/
    crm.yaml        where to connect, and the ceilings
```

```yaml
driver: postgres
secret: CRM_DSN      # the NAME of a tenant secret; its value is a postgres:// DSN
access: read         # read (default) refuses outlet://crm/exec; write allows it
max_rows: 500        # tightens the node ceiling, never raises it
timeout: 5000        # ms; caps every call on this outlet
```

The declaration is code: it deploys with `txco apply` beside the ops that
use it. The credential is not — set it once, as a secret, scoped to the
stack or to the whole tenant:

```sh
txco auth tenant secrets set --stack support CRM_DSN   # prompts for the value
```

Several stacks may declare `secret: CRM_DSN` and share the tenant's
credential; give one of them a stack-scoped `CRM_DSN` with a narrower
database role and it silently uses that instead. Nothing in the ops
changes.

## Calling

```txcl
WHEN @web.req.url.query.email.0 != ""
  EXEC "outlet://crm/query"
    WITH sql = "SELECT id, name, plan FROM customers WHERE email = $1 LIMIT 10",
         args = &array(@web.req.url.query.email.0),
         into = "_crm"
```

`query` runs the statement in a `READ ONLY` transaction; `exec` (on an
outlet declared `access: write`) may change rows and runs in an explicit
transaction. Both take `sql`, `args` (positional binds for `$1`, `$2` …),
`into` (default `_outlet`, private to the flow) and `timeout`.

A read lands at `into`:

```json
{"_crm": {"ok": true, "outlet": "crm",
  "columns": ["id", "name", "plan"],
  "rows": [{"id": 123, "name": "Alice", "plan": "team"}],
  "count": 1}}
```

`columns` gives SELECT order, which object keys don't. A write returns
`{"ok": true, "outlet": "crm", "rows_affected": 1}`, plus `columns`, `rows`
and `count` when the statement has a `RETURNING` clause.

Every failure is data at the same path, and the op still merges:

```json
{"_crm": {"ok": false, "outlet": "crm",
  "error": {"code": "txco_outlet_timeout", "message": "outlet operation timed out"}}}
```

Gate the next op on `._crm.ok == true`. A missing path compares as false,
so gate an error responder on `._crm.error.code != ""` rather than on
`ok == false`, or it fires on requests where the lookup never ran.

| code | meaning |
|---|---|
| `txco_outlet_not_declared` | the outlet isn't in the stack's declaration |
| `txco_outlet_missing_secret` | the named secret isn't set for this stack or tenant |
| `txco_outlet_invalid_request` | an argument the driver can't bind, a bad `into`, or `exec` on a read outlet |
| `txco_outlet_result_too_large` | the row or byte ceiling was crossed; no rows are returned, and an `exec` rolled back |
| `txco_outlet_outcome_unknown` | the connection was lost while a write was committing; verify before retrying |
| `txco_outlet_connect_failed` | DNS, TLS, refused, or blocked by the egress policy |
| `txco_outlet_auth_failed` | the database rejected the credentials |
| `txco_outlet_timeout` | deadline passed, pool wait included |
| `txco_outlet_constraint` | constraint violation (SQLSTATE class 23) |
| `txco_outlet_query_failed` | any other database error; `sqlstate` included |
| `txco_outlet_unavailable` | shutdown, failover, too many connections |

Messages are fixed strings; the database's own error text, which names
hosts and users, never reaches the envelope.

### Arguments and types

`args` accepts `null`, booleans, strings, numbers, and a homogeneous array
of those — so `WHERE id = ANY($1)` takes a list of any length. An integral
number binds as an integer; nulls are allowed anywhere in an array.

Results map deterministically, with no silent precision loss: `int8` beyond
±2^53 and `numeric` arrive as strings, `json`/`jsonb` as the JSON value,
`bytea` as base64, dates and timestamps as strings (`2006-01-02`, RFC 3339
in UTC for `timestamptz`), arrays as arrays, `uuid`/`inet` as strings.

## What apply does

Nothing connects at apply time: a database that is unreachable right now
must never block a deploy. What apply checks it checks from source alone,
and the chassis checks it again before activation:

- every `OUTLETS/<name>.yaml` parses and names a driver built into the chassis;
- every `EXEC "outlet://…"` names an outlet its own stack declares;
- `outlet://<name>/exec` is refused on an outlet declared `access: read`;
- `sql` is a string literal in the op — not a path, not `&concat(...)`.
  Values reach the statement only through `args`, and the statement a stack
  runs against an external system is readable in its source;
- one statement per call, and the leading verb matches the operation
  (`SELECT`/`WITH` for `query`; `INSERT`/`UPDATE`/`DELETE` also for `exec`;
  never DDL).

That last check is lint, not the boundary. `txco lint` runs the same checks
offline.

## Runtime model

- **`query` cannot mutate.** It runs in `BEGIN READ ONLY`, so the database
  refuses a write even inside `WITH x AS (INSERT … RETURNING …) SELECT …`,
  and the row stays unchanged.
- **`exec` either committed or it didn't.** The statement runs in an
  explicit transaction: begin, execute, read the returned rows, check them
  against the ceilings, commit. A ceiling crossed by the returned rows rolls
  back, so `ok: false` means the chassis did not commit. The one case it
  can't settle — a connection lost while `COMMIT` is in flight — is
  `txco_outlet_outcome_unknown`, which the chassis never retries.
- **A ceiling is an error, never a prefix.** A result over the row or byte
  ceiling comes back with no rows at all, so nothing can be mistaken for a
  complete answer. Put `LIMIT` in the SQL.
- **The database role is the boundary.** The role behind the DSN decides
  what an outlet can reach; give it only the tables and verbs the stack
  needs.
- **Every dial goes through the egress policy.** The default `private`
  policy blocks loopback, private and link-local address space, so a DSN
  cannot point the chassis at internal services. `txco dev` runs the
  policy `open` so a local database works; a self-hosted `txco serve`
  needs `--egress-allow-cidrs` to reach a database on its LAN.
- **Pools are per node, and idle at zero.** Nothing opens until an op
  runs; a pool closes after `--outlet-idle-close` unused. A database sees
  roughly nodes × `--outlet-pool-max-conns` connections. Rotating the
  secret or redeploying the declaration switches to a new pool without a
  restart. The driver's execution mode works behind transaction-mode
  poolers; prefer the provider's pooler.
- **The trace records the call, not the data.** Outlet, driver, operation,
  a fingerprint of the statement, duration, rows, bytes and the error code
  — never the SQL text, the values, the rows or the DSN.

## Limits

| knob | default | what it bounds |
|---|---|---|
| `--outlet-max-rows` | 500 | rows per result (a declaration's `max_rows` clamps under it) |
| `--outlet-max-bytes` | 1 MiB | bytes per result, measured while rows stream |
| `--outlet-pool-max-conns` | 4 | connections per outlet per node (minimum is always 0) |
| `--outlet-pool-idle` | 60s | how long an unused connection stays open |
| `--outlet-idle-close` | 5m | how long an unused pool stays open |

Calls run under the ordinary per-op timeout (`WITH timeout` and the
declaration's `timeout` can only lower it). Rows returned are charged fuel
per MiB, like notebook reads.

A worked example lives at `examples/outlet-postgres/`: a lookup by email,
a probe that runs without a database (the missing secret comes back as
data), and a README for running it against a real one.
