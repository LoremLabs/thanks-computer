# State — Durable records with atomic transitions

_KV holds a value; a notebook holds history; `state` holds a **named state
with a version**, changes it only by compare-and-swap, and tells your stack
about every change it makes._

A state record is `(machine, id)` inside your tenant: a `state`, a
`version` that goes up by one on every transition, and opaque JSON `data`.
A transition succeeds only when the record is in the state you say it is
in **and** at the version you last saw, so two actors racing on the same
record cannot both win, and a stale actor (a timer armed against an older
version) is refused rather than applied. Every committed transition writes
a durable event in the **same transaction**, and the chassis presents that
event into your tenant's `_state` stack — at least once, in version order
per record — even across a crash.

It is not a workflow engine. There are no machine definitions, no timers,
no retries of your work, no watchers and no deletion. It is the small
primitive those get built on: pair it with [scheduled](./scheduled.md) for
timers and a notebook for history.

## Records

- **`machine`** — the family the record belongs to, which plays the role a
  KV namespace does: `[a-z][a-z0-9._-]{0,63}`, e.g. `onepony.task`. There
  is no namespace argument.
- **`id`** — free text inside the machine, up to 256 bytes with no `/` or
  control characters, e.g. `paris:t_0193f`.
- **`state`** — a token, `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`.
- **`version`** — starts at 1 and increments on every transition.
- **`data`** — any JSON, up to `--state-max-data-bytes` (64 KiB). Opaque to
  the chassis; it rides the record, not the event.

The tenant is always the request-pinned tenant, never an argument.

## Ops

Output lands under `into` (default `_state`). Errors are in-band —
`<into>.error.{code,message}` with the run continuing — so a conflict is
ordinary control flow.

| op | WITH | result at `into` |
|---|---|---|
| `txco://state/create` | `machine, id, state, data?` | `{ok:true, record}` · `error.code=txco_state_exists` + `current` |
| `txco://state/get` | `machine, id` | `{found:true, record}` or `{found:false}` |
| `txco://state/transition` | `machine, id, from, to, expected_version, data?` | `{ok:true, record, event_id}` · `error.code=txco_state_conflict` with `error.reason = version \| state` + `current` · `txco_state_not_found` |

`record` is `{machine, id, state, version, data, created_at, updated_at}`.

```txcl
# start a task
EXEC "txco://state/create" WITH
  machine = "onepony.task", id = ._task.id, state = "working",
  data = { owner: ._task.owner }

# … later, hand it to a timer: arm first, then move
EXEC "txco://schedule" WITH
  idempotency_key = "task:" + ._task.id + ":v" + (._st.record.version + 1),
  schedule_at = ._task.wake_at,
  payload = { task: ._task.id, version: ._st.record.version + 1 }
EXEC "txco://state/transition" WITH
  machine = "onepony.task", id = ._task.id,
  from = "working", to = "waiting", expected_version = ._st.record.version
```

- **`from` and `expected_version` are both required.** A timer armed at
  `waiting@14` carries 14 and can never wake `waiting@17`: the record moved
  on, and the transition answers `txco_state_conflict` with
  `error.reason = "version"` and the record as it is in `current`.
- **`create` writes no event.** Creation is not a transition.
- **`data` on a transition is optional.** Omit it to keep the stored data;
  give any value (even `{}`) to replace it.
- **`from` may equal `to`.** A data-only change is still a transition: the
  version bumps and an event is presented.
- Other codes: `txco_state_no_tenant`, `txco_state_invalid_arg`,
  `txco_state_store`, and `txco_state_disabled` on a node with no store.

## Firing

Every committed transition is presented into your tenant's **`_state`**
stack (define one to receive them — the stack's existence is the
subscription, like `_scheduled`). The transition rides on `@state.*`:

```txcl
# _state/0/route.txcl
WHEN @state.machine == "onepony.task" && @state.to == "ready"
EXEC "txco://route" WITH stack = "run-task"
```

| Field | Meaning |
|---|---|
| `@state.machine` / `@state.id` | the record |
| `@state.from` / `@state.to` | the transition |
| `@state.version` | the version the transition produced |
| `@state.event_id` | the event's id, stable across re-presentations |
| `@state.attempt` | 1 the first time; higher when this event is being presented again |
| `@state.cause.{source,stack,trace,run}` | which run committed it: its `@src`, its stack, its `@rid`, and its continuation run id when it had one |
| `@state.committed_at` / `@state.fired_at` | when it was committed / presented, UTC |
| `@state.tenant` / `@state.node` | subscriber / presenting chassis |

The record's `data` is not on the event; read it with `txco://state/get`
if the handler needs it, and compare `@state.version` with what comes back —
the record may already have moved on.

## Delivery

Events live in the same store as the records (`--state-store`, default
`sqlite`), written in the transition's own transaction, so an event exists
exactly when its transition does. A node running the `state` personality
**claims** each due event and presents it onto the bus. The claim closes
the moment the chassis has **accepted** the event — routed it into your
tenant and admitted it, just before `_state/0` runs — not when the run
finishes. A long handler never holds a delivery claim open.

- **At least once.** An event the chassis did not accept within
  `--state-accept-timeout` goes back to the queue and is presented again,
  with `@state.attempt` raised; if the first presentation was accepted late
  after all, your stack sees the event twice. Key idempotent work on
  `@state.event_id`, or on `(machine, id, version)`.
- **In order, per record.** Version 9 is never presented before version 8
  has been accepted (or skipped), even across nodes. Their *runs* may
  overlap, because acceptance is not completion.
- **Across a crash.** A claim left stranded by a node that died is
  re-presented after `--state-stale-after`, with the same `event_id`.
- **A crash loop stops.** After `--state-max-attempts` timed-out or
  reclaimed presentations the event is marked `dead` and logged. An
  admission denial (a suspended tenant, a rate limit) is not counted; the
  event waits and is offered again.
- **No stack, no event.** A tenant with no active `_state` stack has its
  events skipped, and they are not replayed if one appears later.
- Outcomes are visible in the logs; terminal rows are kept for
  `--state-retention` and then purged. Pending rows are never purged.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--state-store` | `sqlite` | Backend for the records and their events |
| `--state-db-path` | `./chassis/data/state.db` | Bundled SQLite store path |
| `--state-period` | `5` | Seconds between poll passes; a node presents its own commits at once |
| `--state-max-inflight` | `32` | Concurrent presentations per pass |
| `--state-accept-timeout` | `30` | Seconds to wait for acceptance before re-queueing (counted) |
| `--state-run-timeout` | `600` | Ceiling on a `_state` run once accepted |
| `--state-stale-after` | `300` | Seconds before an abandoned claim is re-presented; keep it above the accept timeout |
| `--state-max-attempts` | `5` | Counted presentations before an event is dead |
| `--state-retention` | `2592000` | Seconds to keep terminal (done/skipped/dead) events (30d) |
| `--state-max-data-bytes` | `65536` | Cap on a record's `data` |

Enable by adding `state` to `--personalities`. With the bundled SQLite
store the ops answer `txco_state_disabled` on a node that does not run the
personality — a record no dispatcher would present is refused rather than
stranded; a shared backend opens on every node.
