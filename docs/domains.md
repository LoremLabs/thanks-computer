# Domains — delegate a subdomain, skip the hard parts

_Thanks, Computer can run the nameserver for a subdomain you delegate
to it — and synthesize everything a working, trustworthy address
needs. ([Overview](./overview.md))_

Receiving email at your own domain is normally where projects stall: an
MX record, an SPF record, a DKIM key minted and published, a DMARC
policy, TLS certificates — each one a chance to be wrong, all of it a
mail admin's afternoon. TxCo's answer: delegate one subdomain, and the
chassis answers its DNS for you.

You may be familiar with setting up nameservers for your domain to get your email working; this is the same process, but for a subdomain. For example, if you want to receive mail at `ai.example.com`, you create NS records for `ai.example.com` pointing to the chassis's nameservers. From then on, the chassis handles all DNS queries for that subdomain, including mail routing and reputation records.

```sh
txco dns zone create ai.example.com
# → add at your registrar:
#     ai.example.com.  NS  ns1.your-chassis.example.
#     ai.example.com.  NS  ns2.your-chassis.example.
```

That NS record is the last DNS you touch. From then on,
`support@ai.example.com` is a programmable address and
`ai.example.com` is a programmable host — backed by rules you write.

## What gets handled

For a delegated zone, the chassis synthesizes — and keeps current —
the records you'd otherwise hand-maintain:

| Concern | What's synthesized |
|---|---|
| Receiving mail | **MX** for the zone (and per-stack hosts) |
| Sender reputation | **SPF** derived from your edge; a **DKIM** keypair minted at zone creation, the public key published, the private key used to sign your [outbound mail](./advanced/protocols/sendmail.md); a **DMARC** record |
| Web | **A/AAAA** for the zone apex and for each active stack (`support.ai.example.com` → your `support` stack) |
| TLS | Wildcard certificates for the zone, issued and renewed automatically via ACME DNS-01 against the chassis's own nameserver |

Records follow your state: activate a stack and its hostname resolves;
the same tables that drive [routing](./advanced/protocols/routing.md)
drive the answers. `txco dns render` previews the full zone before you
delegate, and `txco dns record add` overrides any single record when
you need to.

## Mail in, rules fire

Mail to any address in the zone lands in your tenant's `_mail` stack —
where a [resonator](./txcl.md) classifies it, an
[AI op](./ai.md) drafts, a human [approves](./tutorial.md). The address
isn't a mailbox to poll; it's an entry point to a flow. (Inbound
delivery runs through a standard mail edge in front of the chassis —
the [LMTP reference](./advanced/protocols/lmtp.md) has the wiring.)

## Why this is the deployment story

A working operational endpoint usually means coordinating three
systems — DNS, mail, certificates — owned by three tools. Delegation
collapses them into one: the chassis already knows your stacks, your
hostnames, and your keys, so it can answer for all three without you
keeping them in sync. Self-hosting, you run the nameserver pair and a
mail edge once, then every new zone is one command
([operator reference](./advanced/protocols/dns.md)); on the hosted
cloud, the delegation is the whole job.
