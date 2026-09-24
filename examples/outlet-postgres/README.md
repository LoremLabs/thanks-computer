# outlet-postgres — read the database you already have via `outlet://`

A stack that looks a visitor up in the tenant's own PostgreSQL. The stack
declares the connection once, in `OUTLETS/crm.yaml`; the op writes its own
SQL with every value bound as a parameter; the chassis holds the DSN, owns
the pool, bounds rows, bytes and time, and hands the op JSON. The op never
sees a DSN, a password or a socket.

```
OPS/crm-demo/
  OUTLETS/
    crm.yaml         driver, the NAME of the secret holding the DSN, access, ceilings
  100/lookup.txcl    EXEC "outlet://crm/query" WITH sql = "SELECT … WHERE email = $1", args = [?email]
  200/found.txcl     WHEN ._crm.ok == true  → 200 {email, count, customers}
  200/error.txcl     WHEN ._crm.error.code != "" → 502 {error: txco_outlet_*, message}
  200/usage.txcl     no ?email= → 400
```

## Run it against a real database

```sh
# 1. A database with a customers table (any reachable PostgreSQL; local is fine under `txco dev`).
createdb crm
psql crm <<'SQL'
CREATE TABLE customers (id int8 PRIMARY KEY, email text UNIQUE, name text, plan text);
INSERT INTO customers VALUES (1, 'alice@example.com', 'Alice', 'team'), (2, 'bob@example.com', 'Bob', 'free');
SQL

# 2. Start the chassis from this directory, then hand it the DSN as a stack-scoped secret.
txco dev
txco auth tenant secrets set --stack crm-demo CRM_DSN   # value: postgres://localhost:5432/crm?sslmode=disable

# 3. Ask.
curl 'http://localhost:8080/customers?email=alice@example.com'
# {"email":"alice@example.com","count":1,"customers":[{"id":1,"name":"Alice","plan":"team"}]}
curl 'http://localhost:8080/customers?email=nobody@example.com'
# {"email":"nobody@example.com","count":0,"customers":[]}
```

Without the secret the same request answers `502 {"error":"txco_outlet_missing_secret",…}`:
every failure — secret unset, database down or refusing, credentials
rejected, a row or byte ceiling crossed, a timeout — is data at `into`, and
the next scope branches on `._crm.ok`. Nothing is truncated: a result over
`max_rows` (or `--outlet-max-bytes`) is `txco_outlet_result_too_large` with
no rows, so put `LIMIT` in the SQL.

## What apply checks

`txco apply` (and `txco dev`, and the chassis again at activation) refuses,
from stack source alone: an `outlet://` op naming an outlet the stack
doesn't declare; `outlet://crm/exec` while the outlet says `access: read`;
a `sql` that isn't a string literal (no `&concat`, no path — values go
through `args`); more than one statement; a leading verb that doesn't match
the operation. Nothing connects at apply time; `txco outlet check` is a
later phase.

## What keeps it safe

- `query` runs in `BEGIN READ ONLY`: Postgres refuses a mutation, even one
  hidden in `WITH x AS (INSERT … RETURNING …) SELECT …`.
- `exec` (on an `access: write` outlet) is always an explicit transaction
  that rolls back when a ceiling is crossed, so `ok: false` means the
  statement did not commit. A connection lost mid-`COMMIT` is
  `txco_outlet_outcome_unknown`, never retried automatically.
- Every dial goes through the egress guard. `txco dev` runs the policy
  `open` so a local database works; a self-hosted `txco serve` defaults to
  `private` and needs `--egress-allow-cidrs` to reach a LAN database.
- The database role behind the DSN is the real boundary: give it only the
  tables and verbs the stack needs.

## Operating notes

- Pools are per node: a database sees roughly nodes × `--outlet-pool-max-conns`
  (default 4) connections, minimum 0. Prefer the provider's pooler; the
  driver's execution mode works behind transaction-mode poolers.
- The fleet has no fixed outbound address, so a database that allowlists by
  IP can't allow it yet.
- The trace records the outlet, driver, operation, a fingerprint of the
  statement, duration, rows, bytes and the error code — never the SQL text,
  the values, the rows or the DSN.
