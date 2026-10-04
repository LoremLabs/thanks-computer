# Usage — one record per request

_Every completed request leaves one usage record: who it was for, what came in
and went out, how long it took, how much [fuel](./fuel.md) it spent. The chassis
only records; counting, quotas and billing belong to whatever reads the
records._

## The record

By default each record is a structured log line with the message `usage`,
written at info level:

| Key | What |
|---|---|
| `rid` | The request id, the same one its [trace](./trace.md) has |
| `tenant` | The tenant the request was routed to |
| `src` | The inlet: `http`, `tcp`, `cron`, … — or `compute` / `workspace` for the extra records below |
| `stack` | The stack the request ran, or the entry stage when nothing routed it |
| `duration_ms` | Wall-clock time |
| `status` | `ok` or `error` |
| `bytes_in`, `bytes_out` | Sizes of the request and the response |
| `fuel` | Fuel spent; left off when none was |
| `web.host` | The hostname the client asked for; one stack can serve several. Web requests only |
| `principal` | Who the request acted as, when someone signed in (a mail client, a printer) |
| `mem_bytes` | Peak memory of a nano-op, on `compute` records |
| `admission_denied`, `admission_reason` | The request was turned away before its stack ran: `rate_limited`, `at_capacity`, `suspended`, `payment_required` or `draining` |
| `billable` | `false` on a turned-away request, whose fuel is zero; left off otherwise |

A nano-op and a workspace op each add a record of their own beside the
request's (`src` `compute` or `workspace`, with `duration_ms`), so the machine
time they used can be priced apart from the request. An attached terminal or
service writes a `workspace` record per lease heartbeat for as long as it stays
open, since it outlives the request that opened it.

Requests that never reached a tenant — the chassis's own health checks, unrouted
404s — are logged at debug level under the system tenant, so they stay out of
production logs and out of anyone's usage.

## Turning it off, or sending it elsewhere

`--usage-enabled=false` stops the records. `--usage-sink` picks where they go;
open core ships `zap`, the log line above. A build can register another sink by
name (a queue, a database) without changing the records.

Records are written whether or not tracing is on; a trace is how you see what a
request did, a usage record is how you count it.
