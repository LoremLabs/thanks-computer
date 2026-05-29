# Ingress

*For operators hosting more than one tenant, or binding custom domains to stacks.*

The ingress router is the chassis's first-class entry point. It runs **before any resonator fires** and decides two things per event:

1. **Which tenant** the event belongs to.
2. **Which op stack** to enter.

If no ingress is configured, the chassis falls through to the existing `boot/%/0` entry. Single-tenant deployments don't need an ingress file; everything keeps working exactly as it did before this layer existed.

## The YAML

Default file location is `./ingress.yaml`, overridable with `--ingress-config /path/to/file.yaml` or `TXCO_INGRESS_CONFIG`.

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

On a hit, the chassis writes three fields:

| Field | Meaning |
|---|---|
| `_txc.tenant` | The `tenant:` value from the matched entry. |
| `_txc.stack` | The `stack:` value from the matched entry. The chassis enters `<stack>/0`. |
| `_txc.ingress` | The matched key (`tenant1.example.com`, `smtp-in`, `nightly-reconcile`). For observability — log it, switch on it, ignore it. |

Rule authors read these fields like any other `_txc.*` envelope field. They don't write them — the chassis owns the namespace.

## What gets matched

For v1, matching is exact-string lookup keyed off the source:

| Source (`_txc.src`) | Matched against | Field on envelope |
|---|---|---|
| `http` | HTTP Host header (with port if non-standard) | `_txc.web.req.host` |
| `tcp`  | TCP listener name | `_txc.tcp.listener` |
| `cron` | Cron job name | `_txc.cron.job` |
| `lmtp` | Each RCPT TO independently — exact addr / `@domain` / Strategy A parse / verified domain / listener | per-rcpt; details in [`docs/lmtp.md`](lmtp.md) |

`_txc.web.req.url.path` is captured into the RouteKey for HTTP but **not** matched in v1 — path-prefix routing arrives in a follow-up without changing this YAML shape.

LMTP routing is its own beast: each `RCPT TO` resolves to its own `(tenant, stack)`, recipients that match the same target get batched into one envelope, and cross-tenant deliveries fan out into one envelope per tenant. The full resolution model + the two default strategies (operator-host parsing + verified-domain bypass) are documented in [`docs/lmtp.md`](lmtp.md).

## Fallback when nothing matches

If the request's signal isn't in the YAML — say someone curls a host you didn't configure — the chassis falls back to the legacy `boot/%/0` entry. No tenant fields are stamped. Any txcl boot rule that handled the event before this layer existed will still handle it.

This is the migration-friendly default. To make a deployment strictly multi-tenant, write boot rules that gate (or halt) anything without `_txc.tenant`. There's no `fail-closed` flag in v1 — it's a one-line rule.

## Relationship to `boot/` opstacks

The `boot/<...>` opstacks (the txcl router pattern documented in [quickstart.md](./quickstart.md)) **still work**. They handle the fall-through path. Once you've moved a tenant into ingress, its boot rules become unreachable — the chassis enters the resolved stack directly. You can:

- Leave the old boot rules in place during migration (they're cheap; they just never fire for ingress-routed events).
- Delete them once every tenant is in the YAML.
- Keep one as a catch-all for unrouted traffic.

## What's NOT in v1

Deliberately deferred:

- **Path-prefix matching** for HTTP. Captured in `RouteKey.Path` so the YAML shape stays the same when it lands.
- **Header-based matching** for HTTP.
- **TCP multi-listener config**. v1 has a single listener with hardcoded name `default`.
- **TCP SNI / TLS termination**.
- **Live reload**. Edit the YAML → restart the chassis. (Once the DB-backed resolver below exists, the YAML resolver will likely stay restart-only; mutation is the DB resolver's job.)

## Evolution: from YAML to DB-backed

The `Resolver` interface in `chassis/server/ingress/router.go` is the seam:

```go
type Resolver interface {
    Resolve(key RouteKey) (RouteTarget, bool)
}
```

The YAML loader is one implementation. A follow-up will add a SQLite-backed resolver populated via admin-API mutations — same external-event-log → SQLite pattern that `ops` and `actors` already use. The bus loop calls `resolver.Resolve(key)` and doesn't know or care where the data came from, so swapping in the DB-backed resolver doesn't touch any inlet or rule.

| Deployment shape | Source of routes |
|---|---|
| Single-tenant OSS | No ingress.yaml. Boot routing only. |
| Small multi-tenant OSS | Hand-edited `ingress.yaml`. Restart to change. |
| Chassis-as-service | DB-backed resolver. Admin API mutates. Hot reload via `dbcache` watch pattern. |

## Relationship to auth

The auth schema's `actors.tenant` column (renamed from `actors.org` in 0006) is the same word as `_txc.tenant`. v1 only renames the column so the naming aligns; nothing enforces "actor X can only edit tenant X's stacks" yet. That gating lands as a separate plan, layered on top of this one.
