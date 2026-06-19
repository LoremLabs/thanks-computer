# Routing 

Routing in [Thanks, Computer](https://www.thanks.computer) is multi-tenant, multi-protocol.


## The Ingress

Each protocol translates its request into a standardized input ingress envelope, and then hands it off to the ingress router for processing.

The ingress router is the chassis's first-class entry point. It runs **before any resonator fires** and decides two things per event:

1. **Which tenant** the event belongs to.
2. **Which op stack** to enter.

If no routing is configured, the chassis falls through to the `boot/%/0` entry. [Single-tenant deployments need none of this; everything works out of the box.]

There are two route sources, both live: **hostname bindings in the database** (mutable at runtime, no restart) and an optional **static YAML file** (restart to change). YAML wins on a conflict; the DB is consulted on a YAML miss.

## Hostname DNS bindings

Assign your hostname to a stack:

```sh
txco auth tenant hostnames add acme.example.com 
```

This will ask you to:

```text
Add this TXT record to your DNS:
    _txco-verify.acme.example.com.  TXT  "txco-verify=tcv_XS6ZBUZDKSX6VR36D4ETD4BESTCLS5W46"
```

Then run: 

```sh
txco auth tenant hostnames verify acme.example.com
```

Rows land in the `tenant_hostnames` table (mutated via this CLI or the [admin API](./advanced/admin-api.md)); new bindings are visible on the next request — no restart. Rows carry ownership verification (DNS / HTTP-01 challenges via `txco auth tenant hostnames challenge`); with `--require-hostname-verification=true` unverified rows don't route. `--dev-auto-verify-local-hostnames` (default `true`) auto-verifies `localhost`-style names for development.

## Static YAML bindings

Alternatively you can set the file path with `--ingress-config /path/to/file.yaml` or `TXCO_INGRESS_CONFIG`; the default is empty (no YAML layer). Hand-edit it when your routes are few and you want them visible in one reviewable file.

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
| `lmtp` | Each RCPT TO independently — exact addr / `@domain` / verified domain / listener | per-rcpt; [details](./advanced/protocols/lmtp.md) |

Each protocol head has its own routing peculiarities, for instance, in LMTP routing each `RCPT TO` resolves to its own `(tenant, stack)`, recipients that match the same target get batched into one envelope, and cross-tenant deliveries fan out into one envelope per tenant. 

## Fallback when nothing matches

If the request's signal doesn't match a known tenant — say someone curls a host you didn't configure — the chassis falls back to the legacy `boot/%/0` entry. No tenant fields are stamped. Any txcl boot rule that handled the event before this layer existed will still handle it. This may be fine if you're not using a multi-tenant chassis.

This is the migration-friendly default (`--ingress-miss-action=fallthrough`). For deployments that route everything via hostname bindings, set `--ingress-miss-action=reject`: unmatched requests get a clean HTTP 404 without ever reaching the processor.

## Relationship to auth

`_txc.tenant` is the same tenant the admin API scopes by: admin endpoints live under `/v1/tenants/{tenant}/…`, and tenant membership (who may mutate a tenant's stacks and hostnames) is managed via the members endpoints. The full isolation model — what's tenant-scoped, pinning, memberships — is [tenants.md](./tenants.md); the API detail is [admin-api.md](./advanced/admin-api.md).
