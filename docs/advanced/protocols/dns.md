# DNS — Bring your domain to TxCo

_The `DNS` personality makes the chassis the authoritative nameserver for a
subdomain you delegate to it — synthesizing the mail, web, reputation, and TLS
records a working address needs, from chassis state._

Receiving email at your own domain is normally where projects stall: an MX
record, an SPF record, a DKIM key minted and published, a DMARC policy, TLS
certificates — each one a chance to be wrong, all of it a mail admin's
afternoon. TxCo's answer: delegate one subdomain, and the chassis answers its
DNS for you.

You may already have set up nameservers for a domain to get email working; this
is the same process, but for a subdomain. To receive mail at `ai.example.com`,
you create NS records for `ai.example.com` pointing to the chassis's
nameservers — and from then on the chassis answers every DNS query for that
subdomain. Unlike a normal nameserver where you set individual records, it
handles the entire zone, synthesizing `MX`, `A`, and the rest as your
operations need them — so you can stand up automated email at the app level
under your subdomain.

```sh
txco dns zone create ai.example.com
# → add at your registrar:
#     ai.example.com.  NS  ns1.your-chassis.example.
#     ai.example.com.  NS  ns2.your-chassis.example.
```

That NS record is the last DNS you touch. From then on,
`support@ai.example.com` is a programmable address and `ai.example.com` is a
programmable host — backed by rules you write.

## What gets handled

For a delegated zone, the chassis synthesizes — and keeps current — the records
you'd otherwise hand-maintain:

| Concern | What's synthesized |
|---|---|
| Receiving mail | **MX** for the zone (and per-stack hosts) |
| Sender reputation | **SPF** derived from your edge; a **DKIM** keypair minted at zone creation, the public key published, the private key used to sign your [outbound mail](./sendmail.md); a **DMARC** record |
| Web | **A/AAAA** for the zone apex and for each active stack (`support.ai.example.com` → your `support` stack) |
| TLS | Wildcard certificates for the zone, issued and renewed automatically via ACME DNS-01 against the chassis's own nameserver |

Records follow your state: activate a stack and its hostname resolves; the same
tables that drive [routing](../../routing.md) drive the answers. `txco dns
render` previews the full zone before you delegate, and `txco dns record add`
overrides any single record when you need to.

## Mail in, rules fire

Mail to any address in the zone lands in your tenant's `_mail` stack — where a
[resonator](../../resonators.md) classifies it, an [AI op](../../ai.md) drafts,
a human approves. The address isn't a mailbox to poll; it's an entry point to a
flow. (Inbound delivery runs through a standard mail edge in front of the
chassis — the [LMTP reference](./lmtp.md) has the wiring.)

## Enabling DNS Support

Add `dns` to `--personalities`. The txco chassis head then listens on
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
