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
| tcp   | `--tcp-listen-addrs` | `:5050` | Line-delimited JSON. Comma list of `name=addr` or bare `addr`; a named entry sets `_txc.tcp.listener` for ingress routing. `;tls` after the address terminates TLS and routes by SNI hostname (`;self-signed` for dev); `;handler=NAME` speaks another protocol on the listener (see [tcp](protocols/tcp.md#protocol-handlers)). |
| admin | `--admin-addr`       | `:8081` | Mutating API + admin UI — see [admin-api.md](./admin-api.md).          |
| lmtp  | `--lmtp-listen-addrs`| `:2424` | Only binds when `lmtp` is in `--personalities`.                        |
| cron  | `--cron-period`      | `60`    | Seconds between ticks.                                                 |

`--web-tls-addr` (e.g. `:8443`) makes the chassis terminate TLS itself,
obtaining wildcard certificates via ACME DNS-01 against its own DNS
head — requires the `dns` personality and `--acme-email`. Empty
(default) leaves TLS to your reverse proxy.

### Behind a reverse proxy: whose address is it?

Behind a proxy, every HTTP request arrives from the proxy's address. Name
your proxies and the chassis reads the client's address from
`X-Forwarded-For` instead:

```sh
txco serve --web-trusted-proxies "10.0.0.0/8 fd00::/8"   # or TXCO_WEB_TRUSTED_PROXIES
```

- **Trust is the socket peer, never the header.** The header is read only
  when the connection comes from one of those CIDRs (or bare IPs). From
  anyone else it is ignored, so a client cannot choose its own address.
- **Read from the right.** Each proxy appends the address it accepted the
  request from, so the header runs from the client towards the chassis. The
  client is the first address from the right that is not one of your
  proxies. Anything a client typed into the header sits to the left of that
  and is never reached — two proxy hops work, and so does a proxy that
  passes an incoming header along.
- **What uses it.** The per-IP login limit (`--login-rate`) of the calendar,
  contacts, webdav and ipp heads and their login lines; the client address
  those heads and the websocket head put on their events; the `ip` of the
  web access log.
- **Left empty** (the default) nobody is trusted and the client is the
  socket peer. Behind a proxy that means every client shares ONE per-IP
  login budget, and one app retrying a dead password can use it up for
  everybody — set the flag on any node that sits behind a proxy. Only list
  addresses that your proxies alone can connect from.
- An entry that does not parse is logged at start and not trusted. The
  chassis logs the trusted list once when the web head starts.

IMAP and the TCP head get the same fact from the PROXY protocol instead
(`--imap-proxy-protocol`; [imap.md](./protocols/imap.md)). The admin API
never reads the header.

## Data on disk

:::note
All state is local files — back **these** up.
:::

| Path                                | What                                              |
| ----------------------------------- | ------------------------------------------------- |
| `./chassis/data/db/runtime-$env.db` | Runtime SQLite DB (rules, tenants, hostnames)     |
| `./chassis/data/db/auth-$env.db`    | Auth SQLite DB: admin actors, keys, invitations — and your stacks' [users and credentials](./users.md), which every node opens |
| `./chassis/data/kv/`                | KV store (BoltDB by default)                      |
| `./chassis/data/secrets/txco-master.key` | Secret-store master key — back up separately; see the [secret-store runbook](./runbook-secret-store.md) |
| `./chassis/data/continuations/`     | Suspended-run state (`--continuation-store=file`) |
| `./chassis/data/artifacts/`         | Compute artifacts (wasm modules)                  |
| `./data/trace/`                     | Trace output when `--trace-mode` ≠ `off`          |

Roots are configurable (`--db-root-dir`, `--kvstore-addrs`,
`--secret-master-key`, `--trace-dir`, …).

The request path never reads the runtime DB directly: it reads a SQLite
*mirror* of it, rebuilt on every `txco apply` and swapped in atomically.
By default that mirror lives in memory. `--db-mirror-mode=file` keeps it as
a disposable file under `--db-root-dir` instead (`mirror-<pid>-<gen>.db`),
which makes its pages reclaimable page cache rather than fixed RAM and
lets a reload build the next generation on disk instead of holding two
mirrors at once — the trade is a disk read on a cold page, so it suits a
small, single-purpose node (a dedicated DNS head) rather than a busy web
node. Stale files are swept at boot.

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

`--egress-policy` controls what ops may dial out to: `private` (default)
blocks loopback, RFC 1918, link-local, CGNAT, cloud-metadata ranges, and
anything in `--egress-deny-cidrs`; `open` allows any address. The default
is `private` so a chassis running tenant-authored ops can't be steered
into an SSRF against `169.254.169.254` (cloud metadata), the admin API,
or other internal services. Set `--egress-policy=open` (or whitelist a
specific range with `--egress-allow-cidrs`) only when your own rules must
reach internal/localhost services. `txco dev` and `start.sh` opt into
`open` for local development.

## Shutdown

On `SIGTERM` or `SIGINT` the chassis drains before it stops. New work from
peers that come back on their own is refused: web requests get `503` with
`Retry-After` (and `/healthz` answers `503`, so a load balancer moves on),
and LMTP deliveries get `451`, so the sending MTA requeues. The `scheduled`
and `source` pollers stop claiming. Work already in flight keeps running —
requests, a continuation's detached tail, a worker callback's resume — and
so does internal work nobody would retry, such as a scheduled event already
claimed. The chassis waits up to `--shutdown-grace` for all of it to finish,
then cancels whatever is left and stops.

| Flag               | Default | Meaning                                                          |
| ------------------ | ------- | ---------------------------------------------------------------- |
| `--shutdown-grace` | `25s`   | How long in-flight work gets before it is cancelled; `0` cancels at once |

:::note
Give the container a stop timeout above the grace. Docker's default is
`10s`, after which it sends `SIGKILL` mid-drain; in Compose, set
`stop_grace_period` (for example `60s`).
:::

`SIGUSR1` turns the same drain on without stopping the process, and
`SIGUSR2` turns it off — for taking a node out of rotation by hand.

## AI gateway

The `web` head also serves the AI-gateway inlet (`POST /v1/messages`), which forwards to
an upstream model provider after running the request through the tenant's `_llm` stack.
It is per-tenant opt-in — a tenant enables it by authoring an `_llm` stack, so there is no
enable flag. Full reference: [llm-gateway.md](./protocols/llm-gateway.md).

| Flag                       | Default                     | Meaning                                          |
| -------------------------- | --------------------------- | ------------------------------------------------ |
| `--llm-upstream-url`       | `https://api.anthropic.com` | Base URL forwarded to; invalid fails at boot      |
| `--llm-context-max-tokens` | `2000`                      | Estimated-token cap on injected context           |
| `--llm-context-max-items`  | `8`                         | Item cap on injected context                      |

:::note
Context injection needs **both** caps positive. Setting either to `0` disables injection
entirely, and stack-emitted items are then dropped with only a debug log.
:::

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

:::warning
Set `--require-hostname-verification=true` in production. It's `false` by default
(convenient for dev), which means **unverified hostname bindings still route** — a
tenant could route a hostname it hasn't proven it owns.
:::

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
