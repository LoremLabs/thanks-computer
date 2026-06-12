# DNS — authoritative answers for delegated zones

_The `dns` personality makes the chassis the nameserver for zones you
delegate to it, synthesizing mail, web, and reputation records from
chassis state. The story version is [domains.md](../../domains.md);
this page is the operator facts._

## Enabling the head

Add `dns` to `--personalities`. The head listens on
`--dns-listen-addrs` (default `:5354`; port 53 needs root or
`CAP_NET_BIND_SERVICE`). Minimum config, settable by flag or at
runtime via `txco dns config set` (hot-reload, no restart):

| Setting | Flag / `dns config set` | Meaning |
|---|---|---|
| Nameservers | `--dns-nameservers` / `--nameservers` | The NS FQDNs your zones delegate to |
| Edge IPs | `--dns-edge-ips` / `--edge-ips` | A/AAAA targets for zone apex + stack hosts |
| MX host | `--dns-mx-host` / `--mx` (+ `--dns-mx-priority`, default 10) | Where the zone's MX points — your mail edge |
| TTL | `--dns-synth-ttl` (default 60) | TTL on synthesized records |
| SPF override | `--dns-spf` | Replaces the auto-derived SPF |

## Zones

```sh
txco dns zone create ai.example.com     # registers the zone, prints the NS delegation to add
txco dns zone list
txco dns zone delete ai.example.com
txco dns render [ai.example.com]        # zone-file preview of what the head will serve
```

Two modes per zone: **pattern** (default — full synthesis below) and
**manual** (synthesis off; only explicit `txco dns record add` rows are
served). Record overrides (`record add/list/rm`, types
NS/A/AAAA/MX/TXT) layer on top of synthesis in pattern mode.

## What pattern mode synthesizes

For zone `ai.example.com`:

- **Apex**: NS (from config), SOA (serial derived from row timestamps),
  A/AAAA (edge IPs), MX (`--dns-mx-host`).
- **SPF**: TXT auto-derived from the edge IPs + MX host, `~all`
  softfail. Override wholesale with `--dns-spf`.
- **DKIM**: an RSA-2048 keypair is minted at `zone create`; the public
  key is published at `txco._domainkey.ai.example.com` and the private
  key signs the tenant's [outbound mail](./sendmail.md)
  (longest-match: per-structured-host key, then zone key).
- **DMARC**: `_dmarc.ai.example.com` is published as
  `v=DMARC1; p=none` — monitor-only and **not yet configurable**.
- **Per-stack hosts**: each active stack gets
  `<stack>.ai.example.com` A/AAAA + MX, driven by the activations
  table — activate a stack, its hostname resolves.
- **Structured-host suffix**: when the zone is the
  `--structured-host-suffix`, wildcards plus per-host DKIM/DMARC rows
  from `tenant_hostnames` (reputation isolation per minted host).

Anti-amplification response-rate-limiting and EDNS0/TCP-fallback are
built in.

## TLS: ACME DNS-01 against itself

With `--web-tls-addr` and `--acme-email` set, the chassis obtains and
renews **wildcard certificates** (`ai.example.com` +
`*.ai.example.com`) by answering its own `_acme-challenge` TXT
lookups. Challenge writes arrive via RFC 2136 UPDATE, gated by TSIG
(`--dns-update-tsig-key-name` / `--dns-update-tsig-secret`); they live
in a transient challenge store, never in the zone tables. Certs persist
under `--cert-storage-path` (default `./chassis/data/certs`). A front
proxy can instead ask `GET /_txco/tls-ask?domain=<sni>` to gate
on-demand issuance against verified hostnames.

