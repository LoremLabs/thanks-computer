# Scheduled — Run work at a future time

_Cron turns the clock into a tick; `scheduled` turns a single instant into
one event. A rule enqueues "run this, not before `T`" and walks away; the
chassis fires it when `T` arrives._

Where [cron](./cron.md) is recurring and tenant-wide, `scheduled` is a
one-shot you place from inside a flow — a follow-up email next Tuesday, a
reminder in two weeks, the next installment at 08:00.

## Enqueue with `txco://schedule`

Any rule can place an event. The tenant is the request-pinned tenant, so the
event always fires back into the tenant that scheduled it.

```txcl
EXEC "txco://schedule" WITH
  idempotency_key = "drip:reader@example.com:37",   # dedup + cancel handle
  schedule_at     = "2026-06-30T08:00:00Z",         # RFC3339; "not before"
  payload         = .job                            # any JSON object
```

- **`idempotency_key`** is scoped to the tenant. Re-running with the same key
  while the event is still pending **reschedules in place** (new time/payload) —
  no duplicate. Once it has fired, the key is spent.
- **Cancel** a pending event: `EXEC "txco://schedule" WITH idempotency_key = "…", cancel = true`.

## Firing

At its due time the event is delivered into your tenant's **`_scheduled`**
stack (define one to receive them — the stack's existence is the
subscription, like `_cron`). Your payload rides on `@scheduled.payload`:

```txcl
# _scheduled/0/route.txcl
WHEN @scheduled.payload.kind == "drip"
EXEC "txco://route" WITH stack = "send-drip"
```

| Field | Meaning |
|---|---|
| `@scheduled.payload.*` | the JSON object you enqueued |
| `@scheduled.idempotency_key` | the key it was scheduled under |
| `@scheduled.event_id` | store row id |
| `@scheduled.fired_at` | fire instant, **UTC** RFC3339 |
| `@scheduled.tenant` / `@scheduled.node` | subscriber / firing chassis |

## Delivery

Events live in a durable table (`--scheduled-store`, default `sqlite`). The
poller **claims** each due row before firing — a status flip, so a slow fire
can't double-send — and a claim left stranded by a crash is retried after
`--scheduled-stale-after`. A response timeout is treated as fired (at-most-once
bias — the work likely ran). Outcomes are visible in the logs and trace only.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--scheduled-store` | `sqlite` | Backend for the scheduled_events table |
| `--scheduled-db-path` | `./chassis/data/scheduled.db` | Bundled SQLite store path |
| `--scheduled-period` | `20` | Seconds between poll passes (worst-case firing latency past `schedule_at`) |
| `--scheduled-max-inflight` | `32` | Concurrent fires per pass |
| `--scheduled-stale-after` | `600` | Seconds before an abandoned claim is retried |
| `--scheduled-retention` | `604800` | Seconds to keep terminal (done/failed) rows |

Enable by adding `scheduled` to `--personalities`.