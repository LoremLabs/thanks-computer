# Chassis runtime

_Operator reference for `txco serve`: personalities, listeners, data on
disk, and the flags that matter in production._

Every flag can also be set as an environment variable using the
`TXCO_` prefix and underscores (`--ingress-config` ↔
`TXCO_INGRESS_CONFIG`). `--env` (default `dev`) names the environment
and is embedded in database filenames.

## Personalities

`--personalities` selects which heads the chassis boots. Default:
`cron,tcp,web,admin`. Opt-in: `lmtp` (inbound mail — see
[lmtp.md](./protocols/lmtp.md)) and `dns` (authoritative DNS for
delegated zones, required for the built-in ACME TLS path — see
[dns.md](./protocols/dns.md)).

| Head  | Flag                 | Default | Notes                                                                  |
| ----- | -------------------- | ------- | ---------------------------------------------------------------------- |
| web   | `--web-addr`         | `:8080` | Event inlet, plain HTTP. TLS terminates at a front proxy by default.   |
| tcp   | `--tcp-listen-addrs` | `:5050` | Line-delimited JSON. Comma list of `name=addr` or bare `addr`; a named entry sets `_txc.tcp.listener` for ingress routing. |
| admin | `--admin-addr`       | `:8081` | Mutating API + admin UI — see [admin-api.md](./admin-api.md).          |
| lmtp  | `--lmtp-listen-addrs`| `:2424` | Only binds when `lmtp` is in `--personalities`.                        |
| cron  | `--cron-period`      | `60`    | Seconds between ticks.                                                 |

`--web-tls-addr` (e.g. `:8443`) makes the chassis terminate TLS itself,
obtaining wildcard certificates via ACME DNS-01 against its own DNS
head — requires the `dns` personality and `--acme-email`. Empty
(default) leaves TLS to your reverse proxy.

## Data on disk

All state is local files; back these up, not the process.

| Path                                | What                                              |
| ----------------------------------- | ------------------------------------------------- |
| `./chassis/data/db/runtime-$env.db` | Runtime SQLite DB (rules, tenants, hostnames)     |
| `./chassis/data/db/auth-$env.db`    | Auth SQLite DB (actors, keys, invitations)        |
| `./chassis/data/kv/`                | KV store (BoltDB by default)                      |
| `./chassis/data/secrets/txco-master.key` | Secret-store master key — back up separately; see the [secret-store runbook](./runbook-secret-store.md) |
| `./chassis/data/continuations/`     | Suspended-run state (`--continuation-store=file`) |
| `./chassis/data/artifacts/`         | Compute artifacts (wasm modules)                  |
| `./data/trace/`                     | Trace output when `--trace-mode` ≠ `off`          |

Roots are configurable (`--db-root-dir`, `--kvstore-addrs`,
`--secret-master-key`, `--trace-dir`, …).

## Dispatch limits

| Flag                      | Default   | Meaning                                            |
| ------------------------- | --------- | -------------------------------------------------- |
| `--op-timeout`            | `5s`      | Per-op timeout when a rule sets none               |
| `--op-timeout-max`        | `10m`     | Ceiling for any rule's `WITH timeout`              |
| `--op-payload-max`        | `4194304` | Max op payload, bytes (4 MiB)                      |
| `--max-fuel-per-request`  | `100000`  | Fuel budget per request — see [fuel.md](./fuel.md) |
| `--compute-max-memory-mb` | `32`      | Memory cap per sandboxed nano-op                   |
| `--compute-max-wall`      | `250ms`   | Wall-clock cap per nano-op invocation              |

## Network policy

`--egress-policy` controls what ops may dial out to: `open` (default)
allows any address; `private` blocks loopback, RFC 1918, link-local,
CGNAT, cloud-metadata ranges, and anything in `--egress-deny-cidrs`.
Set `private` on any chassis that runs untrusted rules.

## Routing and tenancy

- `--ingress-config` — path to a static `ingress.yaml`; empty (default)
  disables the YAML layer. Hostname bindings in the `tenant_hostnames`
  table work either way — see [ingress.md](../routing.md).
- `--ingress-miss-action` — `fallthrough` (default) sends unmatched
  events to the `boot/%/0` entry; `reject` returns a clean 404 without
  invoking the processor. Use `reject` when everything routes via
  `tenant_hostnames`.
- `--require-hostname-verification` — `false` by default; set `true`
  in production so unverified hostname bindings don't route.
  (`--dev-auto-verify-local-hostnames`, default `true`, auto-verifies
  `localhost`-style names for development.)

## Admin auth

`--auth-mode` is one of `basic`, `signed`, or `both` (default `both`);
`--admin-user` / `--admin-pass` set the basic credentials. With no
basic credentials and no enrolled signing keys, the chassis runs in
open-dev mode (requests get an `admin:all` context, `source: "open"`)
— local development only. Details and the enrolment flow:
[admin-api.md](./admin-api.md).

## Observability

- `--trace-mode` `off` (default) | `summary` | `full`; `--trace-dir`
  (default `./data/trace`); `--trace-async` (default `false`) moves
  trace writes off the request path — see [trace.md](./trace.md).
- Prometheus metrics are exported under the `txco` namespace
  (`--prom-namespace`).
- `--log-ops` (default `disabled`) writes per-op logs to
  `--log-ops-dir`.
