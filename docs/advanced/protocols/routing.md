# Routing — which tenant, which stack

*For operators hosting more than one tenant, or binding custom domains to stacks. For the individual channels, see the [protocols index](./README.md).*

The ingress router is the chassis's first-class entry point. It runs **before any resonator fires** and decides two things per event:

1. **Which tenant** the event belongs to.
2. **Which op stack** to enter.

If no routing is configured, the chassis falls through to the `boot/%/0` entry. Single-tenant deployments need none of this; everything works out of the box.

There are two route sources, both live: **hostname bindings in the database** (mutable at runtime, no restart) and an optional **static YAML file** (restart to change). YAML wins on a conflict; the DB is consulted on a YAML miss.

## Hostname bindings (the dynamic path)

The day-to-day way to route — what the [quickstart](../../quickstart.md) uses:

```sh
txco auth tenant hostnames add acme.example.com --stack acme/web
```

Rows land in the `tenant_hostnames` table (mutated via this CLI or the [admin API](../admin-api.md)); new bindings are visible on the next request — no restart. Rows carry ownership verification (DNS / HTTP-01 challenges via `txco auth tenant hostnames challenge`); with `--require-hostname-verification=true` unverified rows don't route. `--dev-auto-verify-local-hostnames` (default `true`) auto-verifies `localhost`-style names for development.

## The YAML (the static path)

Set the file path with `--ingress-config /path/to/file.yaml` or `TXCO_INGRESS_CONFIG`; the default is empty (no YAML layer). Hand-edit it when your routes are few and you want them visible in one reviewable file.

```yaml
ingress:
  http:
    hosts:
      tenant1.example.com:
        tenant: tenant1
        stack: tenant1/web
      acme.local:
        tenant: acme
        stack: acme/web
  tcp:
    listeners:
      smtp-in:
        tenant: tenant1
        stack: tenant1/mail
  cron:
    jobs:
      nightly-reconcile:
        tenant: system
        stack: system/cron
  lmtp:
    listeners:
      default:
        tenant: system
        stack: system/mail_catchall
    recipients:
      "support@acme.example":
        tenant: acme
        stack: acme/support
      "@acme.example":             # @domain wildcard for the rest
        tenant: acme
        stack: acme/catchall
    verified_domains:              # static stand-in for the
      beta.example:                # chassis's tenant_hostnames DB
        tenant: beta               # `anything@beta.example` →
                                   # `beta/_mail/0` (convention).
                                   # See docs/lmtp.md for the full
                                   # routing model.
```

Each source has its own keyspace — a hostname listed under `http.hosts` will not also match as a `tcp.listeners` entry. Sources are siloed.

## What gets stamped on the envelope

On a hit, the chassis stamps the envelope:

| Field | Meaning |
|---|---|
| `_txc.tenant` | The `tenant:` value from the matched entry. |
| `_txc.stack` | The `stack:` value from the matched entry. The chassis enters `<stack>/0`. |
| `_txc.ingress` | The matched key (`tenant1.example.com`, `smtp-in`, `nightly-reconcile`). For observability — log it, switch on it, ignore it. |
| `_txc.hostname_verified` | Whether the matched hostname has passed ownership verification (see below). |

Rule authors read these fields like any other `_txc.*` envelope field. They don't write them — the chassis owns the namespace.

## What gets matched

Matching is exact-string lookup keyed off the source:

| Source (`_txc.src`) | Matched against | Field on envelope |
|---|---|---|
| `http` | HTTP Host header (with port if non-standard) | `_txc.web.req.host` |
| `tcp`  | TCP listener name (from `--tcp-listen-addrs`, `name=addr` form; bare addrs use `default`) | `_txc.tcp.listener` |
| `cron` | Cron job name | `_txc.cron.job` |
| `lmtp` | Each RCPT TO independently — exact addr / `@domain` / Strategy A parse / verified domain / listener | per-rcpt; details in [`docs/lmtp.md`](lmtp.md) |

LMTP routing is its own beast: each `RCPT TO` resolves to its own `(tenant, stack)`, recipients that match the same target get batched into one envelope, and cross-tenant deliveries fan out into one envelope per tenant. The full resolution model + the two default strategies (operator-host parsing + verified-domain bypass) are documented in [`docs/lmtp.md`](lmtp.md).

## Fallback when nothing matches

If the request's signal isn't in the YAML — say someone curls a host you didn't configure — the chassis falls back to the legacy `boot/%/0` entry. No tenant fields are stamped. Any txcl boot rule that handled the event before this layer existed will still handle it.

This is the migration-friendly default (`--ingress-miss-action=fallthrough`). For deployments that route everything via hostname bindings, set `--ingress-miss-action=reject`: unmatched requests get a clean HTTP 404 without ever reaching the processor.

## Relationship to `boot/` opstacks

The `boot/<...>` opstacks (the txcl router pattern) **still work**. They handle the fall-through path. Once you've moved a tenant into ingress, its boot rules become unreachable — the chassis enters the resolved stack directly. You can:

- Leave the old boot rules in place during migration (they're cheap; they just never fire for ingress-routed events).
- Delete them once every tenant is in the YAML.
- Keep one as a catch-all for unrouted traffic.

## How the two sources compose

The `Resolver` interface in `chassis/server/ingress/router.go` is the seam; the YAML loader and the DB-backed resolver (`db_resolver.go`, snapshotting the live `dbcache` mirror) are its two implementations. Precedence: **YAML wins; the DB is consulted only on a YAML miss.** For LMTP, DB-verified domains layer between the YAML recipient rules and the listener fallback.

| Deployment shape | Source of routes |
|---|---|
| Single-tenant | No routing config. Boot routing only. |
| Small multi-tenant, static | Hand-edited `ingress.yaml`. Restart to change. |
| Dynamic / hosted | `tenant_hostnames` rows via admin API or CLI. Live reload. |

## Relationship to auth

`_txc.tenant` is the same tenant the admin API scopes by: admin endpoints live under `/v1/tenants/{tenant}/…`, and tenant membership (who may mutate a tenant's stacks and hostnames) is managed via the members endpoints. See [admin-api.md](../admin-api.md).
