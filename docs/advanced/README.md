<!-- nav: Operator index -->

# Advanced — operator references

Fact-level documentation for running and operating a chassis. The
[main docs](../README.md) explain the ideas, the
[authoring guides](../authoring/README.md) cover the day-to-day
workflow; these pages list the flags, endpoints, file formats, and
failure modes.

## The chassis

- **[serve.md](./serve.md)** — runtime reference: personalities,
  listeners, data on disk, dispatch limits, key flags.
- **[cli.md](./cli.md)** — the complete `txco` command surface,
  grouped by task.
- **[admin-api.md](./admin-api.md)** — the admin HTTP API: auth modes,
  signed requests, enrolment, tenant-scoped endpoints, versioned
  stacks.
- **[fuel.md](./fuel.md)** — the per-request budget guards: fuel
  metering, TTL hop counting, repeat-transition penalties, the loop
  lint.
- **[runbook-secret-store.md](./runbook-secret-store.md)** — the
  per-tenant secret store: bootstrap, CRUD, rotation, disaster
  recovery.
- **[trace.md](./trace.md)** — trace internals: modes, file layout,
  redaction, the `txco trace` CLI.

## Protocols — one page per channel

- **[protocols/](./protocols/README.md)** — the index:
  [routing](./protocols/routing.md) (tenant + stack resolution),
  [web](./protocols/web.md), [lmtp](./protocols/lmtp.md) (mail in),
  [sendmail](./protocols/sendmail.md) (mail out),
  [cron](./protocols/cron.md), [tcp](./protocols/tcp.md),
  [mcp](./protocols/mcp.md), [dns](./protocols/dns.md) (delegated
  zones).

## For rule authors

- **[envelope.md](./envelope.md)** — every `_txc.*` field you read or
  write, the full `WITH` table, operators, `SET PRE`, `PRIORITY`.
- **[builtins.md](./builtins.md)** — all EXEC schemes and the
  `txco://` builtin registry.
- **[txcl/txcl.md](./txcl/txcl.md)** — the full TXCL language
  reference.
- **[txco-oci-packages.md](./txco-oci-packages.md)** — the package
  system: manifest, lockfile, signing, registries.