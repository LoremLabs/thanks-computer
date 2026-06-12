# Tenants — one chassis, many isolated worlds

_In Thanks, Computer, every piece of state belongs to a **tenant** —
an isolation boundary for stacks, domains, secrets, people, and usage.
This page is the model, and how to set it up. ([Overview](./overview.md))_

You've been using it all along: a fresh chassis seeds a tenant named
`default`, and everything in the [quickstart](./quickstart.md) ran
inside it. That's the design — isolation isn't a feature you turn on
later, it's how the chassis thinks. A single team can ignore tenancy
entirely; the moment you host a second team, a client, or a product,
the walls are already there.

## What lives inside a tenant

Everything, keyed at the database row:

| Resource | The boundary |
|---|---|
| Stacks & rules | A stack name is unique *per tenant*; tenant A's rules never fire for tenant B's events |
| Hostnames & [domains](./domains.md) | Hostname bindings, delegated DNS zones, and DKIM keys are tenant rows — one tenant's claim 409s another's |
| [Secrets](./running.md) | Scoped `(tenant, stack, name)`; materialization can't cross the line |
| [Traces](./trace.md) | Tenant-attributed; the admin API only serves them under `/v1/tenants/{slug}/…` |
| [Cron](./advanced/protocols/cron.md) | Each tenant with a `_cron` stack gets its own tick envelope |
| Usage & [fuel](./advanced/fuel.md) | Every request's spend is attributed to its tenant — the quota/billing dimension |
| Outbound [mail](./advanced/protocols/sendmail.md) | Per-tenant rate limits, and the from-domain must be *that tenant's* verified hostname |
| People | Actors hold *per-tenant* memberships with per-tenant capabilities |

## How an event gets its tenant — and keeps it

[Routing](./advanced/protocols/routing.md) resolves the tenant before
any rule fires — by hostname, mail recipient, or listener — and stamps
`@tenant` on the envelope. Then it's **pinned**: immutable for the
life of the request. Rules and ops can't overwrite it (the envelope
guard rejects the write), and usage accounting reads the pin, not the
envelope — a misbehaving rule can't bill its work to someone else.

Events that match nothing land in the system tenant (`_sys`), where
boot rules may assign them to a real tenant exactly once. Tenant slugs
starting with `_` are reserved for the chassis.

## People: memberships, not global roles

An actor's capabilities are *per tenant* — admin of `acme`, read-only
in `beta`, nothing anywhere else. The admin API enforces this at the
path: under `/v1/tenants/acme/…`, a signed caller's capabilities are
re-resolved from their `acme` membership (no membership, no access),
and a browser session minted for one tenant won't replay against
another. Invitations are tenant-scoped and can't grant capabilities
the inviter doesn't hold. Operators with `super_admin` see across
tenants — that's the one global role.

## Setting one up

```sh
txco auth tenant create acme                                # the tenant
txco auth invite --tenant acme --label "alice"              # token for a teammate
txco auth tenant hostnames add acme.example.com --stack acme/web
txco auth tenant secrets set STRIPE_API_KEY --tenant acme
txco apply --tenant acme                                    # deploy stacks into it
```

`--tenant` defaults to `default` (or your profile's default), so
single-tenant workflows never type it. `txco auth tenants` lists what
you can see; `txco auth memberships` lists where you belong.

## Operating tenants

Suspension is a clean kill switch: `txco admin tenant suspend acme
--deny-status 402 --deny-reason payment_required` makes the chassis
answer that tenant's traffic with your chosen denial — while still
attributing the attempts to the tenant for the record. `resume` lifts
it. Deleting is a soft revoke: routing and cron stop, history and audit
rows remain.

Outbound-mail rate limits are tracked per node (a runaway-loop valve, not fleet-wide accounting), and hostname-ownership verification is permissive by default — set `--require-hostname-verification` in production so unverified bindings don't route.
