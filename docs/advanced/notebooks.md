<!-- nav: Notebooks -->

# Notebooks — an append-only record a stack writes and reads

_`txco://notebook/*` is the one place an op can keep **history**: an
ordered, append-only record that outlives the request. The KV store holds a
stack's current state; a notebook holds how it got there — task activity,
a conversation, a pony's audit trail, an import's diagnostics — read back
by cursor, by time window, or from the tail, and exported as
newline-delimited JSON._

> **Task is state; notebook is history.** A task record answers "where are
> we now?"; its notebook answers "how did we get here?"

A notebook is identified by its **name** inside a **namespace** — `task/42`
in `www`, `conversation/alice` in `pony-paris` — and holds entries:

```json
{"seq": 7, "at": "2026-09-11T14:03:12.481027000Z", "type": "command.finished",
 "data": {"cmd": "npm test", "exit": 0}, "object_key": "run:8f2c"}
```

`seq` is the notebook's own order, allocated on append and never reused;
`at` is assigned by the chassis at commit (a caller's own timestamps go
inside `data`); `type` is a short dotted label; `data` is any JSON;
`object_key` is optional and makes an append idempotent.

## The ops

All five are tenant-scoped and answer at `into` (default `_notebook`).

```txcl
# journal a decision — safe to run again for the same message
WHEN ._gate.audit.source != ""
  EXEC "txco://notebook/append"
    WITH notebook   = &concat("task/", ._p.msgkey),
         type       = "recipient.resolved",
         data       = ._gate.audit,
         object_key = &concat("gate:", ._p.msgkey)
# → _notebook = {seq, at, existed}
```

| op | WITH | result |
|---|---|---|
| `notebook/append` | `notebook`, `type`, `data?`, `object_key?`, `ttl?` (seconds), `namespace?` | `{seq, at, existed}` |
| `notebook/read` | `notebook`, `after?`, `since?`, `until?`, `tail?`, `type?`, `limit?`, `namespace?` | `{entries, next, cursor, count, truncated}` |
| `notebook/export` | `notebook`, `format?` (`ndjson`), `after?`, `since?`, `until?`, `type?`, `limit?`, `namespace?` | the body on `@web.res.body` + `@halt`; `{count, next, cursor, truncated, bytes}` at `into` |
| `notebook/list` | `prefix?`, `after?`, `limit?`, `namespace?` | `{notebooks: [{name, high_seq, ttl_secs, created_at, updated_at}], next, count}` |
| `notebook/delete` | `notebook`, `namespace?` | `{deleted}` |

**A duplicate `object_key` returns the original.** With at-least-once
inlets, mail redelivery and task resume, `(notebook, object_key)` is how a
stack says "this event is already recorded": the second append writes
nothing and answers the original `{seq, at}` with `existed: true` — a retry
is observationally identical to the first success, so caller code needs no
special case.

**Failure is branch-visible.** Errors land as `._notebook.error.{code,
message}` and the run continues; there is never a success response without
a record. A stack that must not declare a task complete unless the
completion was journaled can say so:

```txcl
WHEN ._notebook.error.code != ""
  EMIT ._task.status = "journal_failed"
```

Codes: `txco_notebook_no_tenant`, `txco_notebook_disabled` (no store on
this node), `txco_notebook_invalid_arg`, `txco_notebook_invalid_name`,
`txco_notebook_too_large`, `txco_notebook_stale_cursor`,
`txco_notebook_store`.

## Reading: always oldest first

Selection varies; order never does. **Every read returns entries ascending
by `seq`** — `tail = 50` selects the newest fifty and returns them oldest to
newest, so "the latest entry" is always the last element and a paged read
accumulates in order.

| param | selects |
|---|---|
| `after` | entries after a cursor — the head walk |
| `since` / `until` | entries whose `at` is in `[since, until)` (RFC 3339, any precision) |
| `tail` | the newest N of the selection |
| `type` | only entries of that type |
| `limit` | the page size (default 100, node ceiling `--notebook-max-read-rows`) |

Two fields come back with every page. `next` is the cursor for the
following page and is non-empty **only when the page was full** — the
drain idiom, which the [`LOOP` clause](./txcl/txcl.md) turns into one op:

```txcl
EXEC "txco://notebook/read"
  WITH notebook = "task/42", limit = 200, after = ._notebook.next
LOOP EVERY "2ms" UNTIL ._notebook.next == "" MAX 20
# arrays append on merge, so ._notebook.entries accumulates — in order
```

`cursor` is the position after the last entry returned, whenever there was
one. It is what a poller hands back: `tail = 20` for the first paint, then
`after = ._notebook.cursor` to pick up what arrived since.

**Cursors are opaque.** `after`, `next` and `cursor` are strings the
chassis issues; never build one from a `seq`. A cursor carries the
notebook's generation, so if the notebook is deleted and written again, an
old cursor answers `txco_notebook_stale_cursor` instead of silently reading
new entries as old ones. Start over from the beginning.

## Exporting NDJSON

`notebook/export` writes the selection straight onto the HTTP response —
one entry per line, `content-type: application/x-ndjson` — and halts, so a
stack serves a log file in one op:

```txcl
# 1000_PREFLIGHT — lift the query parameters once
WHEN @src == "http" && @web.req.url.path == "/task/log.ndjson"
  EMIT ._q.task  = @web.req.url.query.task.0,
       ._q.since = @web.req.url.query.since.0
```

```txcl
WHEN ._q.task =~ /^[a-z0-9-]+$/
  EXEC "txco://notebook/export"
    WITH notebook = &concat("task/", ._q.task),
         since    = ._q.since
```

An export is bounded by `limit` rows and `--notebook-max-export-bytes`
(8 MiB); when either cuts it short, `truncated` is true and `next` — also
sent as the `x-notebook-next` response header — resumes it with `after`.

## Naming and scope

- **Names** are `/`-separated segments of `[A-Za-z0-9._-]` (no `.`, `..`,
  or a segment starting with `_`; at most 250 bytes) — the blob grammar, so
  hierarchical families (`task/…`, `conversation/…`, `workspace/…`) work
  and `list WITH prefix = "task/"` enumerates one.
- **Namespace** defaults to the routed stack, with a nested inlet stack
  (`core/_mail`, `core/_websocket`) sharing the app's — the KV rule. Pass
  `namespace` to choose one, as a per-persona stack does with
  `&concat("pony-", ._in.slug)`. `_txc`-prefixed namespaces are reserved.
- **Tenant** is pinned from the request; a notebook is never addressable
  across tenants.

A notebook exists from its first append; `list` and `delete` see heads,
`read` on a name that was never written is simply empty.

## Retention

Unbounded by default. `WITH ttl = 604800` on an append expires that entry a
week later; expired entries disappear from reads at once and are reclaimed
by a periodic sweep (`--notebook-sweep-period`, 10m). Pruning never
renumbers — `seq` only ever grows.

## What a notebook is not

A notebook write is **not** part of the transaction of the thing it
describes: a command can finish while its `command.finished` append fails,
or the reverse. Notebooks are an application record, not an event-sourcing
substrate, and give no atomicity with other chassis ops — never make task
state rebuildable *only* from a notebook. They are not a search index, not
metrics, not a queue, and have no aggregates or cross-notebook queries.

## Operating it

The store is its own database — never the runtime DB — a per-node SQLite
file by default (`--notebook-db-path`), or a shared backend a deployment
registers (the hosted build points every node at one Postgres). Entries
are capped at `--notebook-max-entry-bytes` (64 KiB); a read or export pays
fuel per MiB returned on top of the dispatch cost.

```
txco notebook list www task/                 # notebooks under a prefix
txco notebook tail -n 20 www task/42         # newest twenty, oldest first
txco notebook read --all www task/42         # every entry, one JSON object per line
txco notebook export www task/42 > task-42.ndjson
```

The same view is `GET /v1/tenants/{t}/notebooks/{namespace}[/{name}]` on
the [admin API](./admin-api.md) (`notebook:*:read`).

A worked example lives at `examples/notebook-log/`.
