# Cron — time as a channel

_The cron head turns the clock into events: every `--cron-period`
seconds (default 60), subscribed stacks receive a tick envelope and
their rules decide what the hour demands._

## Subscribing is implicit

Define a stack named **`_cron`** in your tenant and you're subscribed:
each tick, the chassis queries for tenants with a `_cron` stack and
delivers one envelope per tenant (`@cron.job = "_cron"`,
`@cron.tenant = <slug>`). No registration, no YAML — the stack's
existence is the subscription.

## The tick envelope

Rules match on wall-clock fields rather than cron syntax:

```txcl
WHEN @cron.hour == 9 && @cron.minute == 0     # every day at 09:00
EXEC "https://ops.example.com/morning-sweep"
```

| Field | Meaning |
|---|---|
| `@cron.{minute,hour,dom,dow,month,year}` | Wall-clock at the tick |
| `@cron.mod5` / `mod10` / `mod15` / `mod30` | Precomputed buckets — `WHEN @cron.mod15 == 0` fires every 15 minutes |
| `@cron.bucket` | Canonical period-boundary timestamp — the fleet dedup key |
| `@cron.tick` | Monotonic counter since boot |
| `@cron.job` / `@cron.tenant` / `@cron.node` | `_cron` or `default` / subscriber / firing chassis |

Tenant deliveries are **smeared** across the period (a deterministic
per-tenant offset) so a thousand tenants don't stampede at second
zero — but the envelope's clock fields are frozen at the tick instant,
so `WHEN @cron.minute == 0` still means minute zero regardless of when
your delivery lands.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--cron-period` | `60` | Seconds between ticks (min 1) |
| `--cron-max-inflight` | `32` | Concurrent dispatches per tick |
| `--cron-system-tick` | `false` | Enable the tenant-less system tick |
| `--cron-queue` | `local` | Queue backend (in-process; the interface is pluggable for brokers) |

The `local` queue is single-node, at-most-once — no retries, no
cross-node dedup. Fleet-grade scheduling rides on `@cron.bucket` as
the dedup key.
