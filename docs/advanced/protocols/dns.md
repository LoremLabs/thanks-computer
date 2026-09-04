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
| Mail clients | **SRV** `_imaps._tcp` (RFC 6186) at the apex and each stack host when `--dns-imaps-port` is set, so Thunderbird and friends find the [IMAP](./imap.md) server for `paris@support.ai.example.com` without being told |
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
| IMAPS port | `--dns-imaps-port` (default 0 = off) | Publishes `_imaps._tcp` SRV records pointing each name at itself on this port; the default-suffix wildcard zone points at `imap.<suffix>` |

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

Orthogonal to the mode is *who answers*: by default the prebuilt zone
does; `--answer stack` hands each query to your `_dns` stack instead
(see [Answering queries from a stack](#answering-queries-from-a-stack)).
`txco dns zone set <origin> --answer stack|snapshot` flips an existing
zone in place.

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

## Observing queries: the `_dns` stack

Every other head turns its protocol into an event your rules can act on;
DNS does too. Give your tenant a **`_dns`** stack and each query the chassis
answers for one of your zones is delivered into it — *after* the answer has
left on the wire, fire-and-forget. The stack's existence is the
subscription, exactly like `_cron` and `_scheduled`; no `_dns` stack, no
events, no change in how the zone is served.

```txcl
# OPS/_dns/0100_COUNT/count.txcl — query counters by type + outcome
WHEN @dns.phase == "observe"
  EMIT @telemetry.metrics = &array(
    &object("name",  "dns.queries",
            "kind",  "counter",
            "value", 1,
            "attrs", &object("type",  @dns.q.type,
                             "rcode", @dns.reply.rcode)))
```

That is the whole analytics feature: the counters ride the tenant
[telemetry](../../telemetry.md) exporter you already have. Write to
[KV](../kv.md) for rollups, log NXDOMAIN bursts, alert on a name nobody
should be asking for — it's a stack, so it's rules.

| Field | Meaning |
|---|---|
| `@dns.q.name` / `@dns.q.type` / `@dns.q.class` | the question — lowercased FQDN, and mnemonics (`A`, `TXT`, `IN`) |
| `@dns.reply.rcode` | what was answered: `NOERROR`, `NXDOMAIN`, `REFUSED`… |
| `@dns.reply.answer` / `@dns.reply.authority` | the records sent, as zone-file lines (`"shop.ai.example.com.\t60\tIN\tA\t203.0.113.10"`) |
| `@dns.reply.authoritative` / `@dns.reply.truncated` | AA flag; TC set (UDP answer didn't fit) |
| `@dns.client.ip` / `@dns.client.transport` / `@dns.client.edns_udpsize` | who asked, over `udp` or `tcp`; the EDNS0 buffer they advertised (absent without OPT) |
| `@dns.zone.origin` / `@dns.zone.mode` | the served zone the name fell in; `pattern` or `manual` |
| `@dns.phase` | `observe` — a post-reply tap (the only phase today) |
| `@dns.tenant` / `@dns.node` | you; the chassis that answered |

What you will *not* see: names outside any zone you own (the head refuses
those and there is nobody to deliver to — which is also where scanner
noise lives), queries the response-rate-limiter dropped, and RFC 2136
updates. `_acme-challenge` lookups answered from the transient challenge
store **are** observed like any other query.

The tap never touches the answer. `@dns.*` is read-only from a stack, and
a slow or failing `_dns` stack costs the query path nothing: the tap
hands off through a bounded queue and drops (counted in
`chassis.dns.observe`, `outcome=dropped`) rather than ever delaying a
reply. Each observed query is an ordinary run of your `_dns` stack —
fuel-metered like any other — so a busy zone should sample:

| Flag | Default | Meaning |
|---|---|---|
| `--dns-observe-sample` | `1` | `1` observes every answered query; `N` one in N; `0` turns the tap off chassis-wide |
| `--dns-observe-max-inflight` | `8` | concurrent `_dns` dispatches; the hand-off queue behind it holds 1024 |

## Answering queries from a stack

Observing is read-only. A zone can go further and let the `_dns` stack
*decide the answer*:

```sh
txco dns zone set ai.example.com --answer stack            # flip in place
txco dns zone create ai.example.com --answer stack         # or from the start
```

For such a zone the head first works out what it *would* have answered —
the same synthesis and overrides as before — then hands the query to
your `_dns` stack with that answer attached as `@dns.proposed`. Whatever
the stack `EMIT`s under `@dns.res` goes on the wire. The
backwards-compatible stack is one line:

```txcl
# OPS/_dns/0100_ANSWER/passthrough.txcl — answer exactly what the zone would have
WHEN @dns.phase == "answer"
EMIT @dns.res = @dns.proposed
```

Put your own logic in front of it and the proposal stays the default:

```txcl
# OPS/_dns/0100_ANSWER/version.txcl — a TXT served from state, not from a zone file
WHEN @dns.phase == "answer"
WHEN @dns.q.type == "TXT"
WHEN @dns.q.name == "build.ai.example.com."
EMIT @dns.res.rcode  = "NOERROR"
EMIT @dns.res.answer = ["build.ai.example.com. 30 IN TXT \"v1.4.2\""]
```

| `@dns.res` field | Meaning |
|---|---|
| `rcode` | `NOERROR` (default when omitted), `NXDOMAIN`, `SERVFAIL` or `REFUSED` — nothing else |
| `answer` / `authority` | records as zone-file lines, `"<owner> <ttl> IN <TYPE> <rdata>"` — any type `dns.NewRR` parses; a name without a trailing dot is taken as fully qualified |
| `@dns.proposed.{rcode,answer,authority}` | the head's own answer, same encoding — echo it, edit it, or ignore it |
| `@dns.zone.fallback` | this zone's fallback policy (below) |

The head guards what it will put on the wire: every owner must fall
inside the zone, meta types (OPT, TSIG, TKEY) are refused, at most 64
records, TTLs capped at a week. A response that breaks any of these is
rejected *whole* and the zone's fallback answers instead.

**What answers when the stack doesn't.** A stack can be slow, broken,
suspended, or over its dispatch budget. Each zone picks its fallback:

| `--fallback` | Wire answer when there is no valid `@dns.res` |
|---|---|
| `proposal` (default) | what the zone would have said anyway — a bad deploy degrades to today's behavior, not to darkness |
| `servfail` | SERVFAIL, so resolvers retry rather than cache a wrong answer — for zones whose truth lives only in the stack |

Three things keep the lane bounded. An **answer cache** keyed by
(zone, name, type) holds each stack answer for its minimum TTL (negative
answers for the SOA minimum), so steady traffic is served from memory
and your stack sees each distinct question once per TTL. A per-zone
**dispatch limit** caps how many queries per second may reach the stack
at all; the rest answer with the fallback. And a **deadline** bounds how
long the wire waits: past it the fallback answers, but the run is not
cancelled — its late answer warms the cache for the next asker.

| Flag | Default | Meaning |
|---|---|---|
| `--dns-stack-deadline-ms` | `1500` | how long a query waits for `@dns.res` before the fallback answers; keep it under a resolver's own retry timer |
| `--dns-stack-dispatch-per-sec` | `20` | per-zone ceiling on stack dispatches; `0` disables the limiter |

What the stack never sees: names outside the zone (REFUSED), `ANY`
(refused, anti-amplification), `_acme-challenge` lookups during
certificate issuance, and RFC 2136 updates — those stay in the head so
TLS never depends on tenant code. A stack-answered query is not
re-delivered to the observe tap (the stack already saw it); cache hits
and fallbacks are, so analytics stays complete. A zone set to
`--answer stack` whose tenant has no active `_dns` stack keeps serving
the prebuilt zone and logs a warning.

Two caveats worth knowing up front. Stack answers cannot be DNSSEC-signed
offline, so a stack-answered zone is incompatible with signing until
online signing exists. And a `_dns` op that itself resolves a name in a
stack-answered zone served by the same head re-enters as a fresh,
rate-limited, fuel-metered query — answer from state, not from DNS.

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
