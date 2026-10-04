# Rooms — talk to your stacks from a terminal

_A room is a named, shared conversation a tenant's people post into. Each message
becomes an ordinary event, `@src == "room"`, that runs through the tenant's `_room`
stack like a web request or a mail does; what the stack answers is the reply. There is
no separate assistant path: a room message is traced, metered and checked like any
other run._

## Send a message

```sh
thanks --room support "why did ticket 184 fail?"    # one-shot: prints the reply
txco room --room support "why did ticket 184 fail?" # the same; thanks is txco room
thanks --room support                               # the live feed: type to send, Ctrl-D to leave
```

`thanks` is the `txco` binary under a second name. With no `--room` the room is
`general`. `--tenant`, `--profile` and `--addr` pick the tenant and chassis as the
other commands do.

Over the admin API, a message is `POST /v1/tenants/{tenant}/rooms/{room}/messages`
and the feed is `GET /v1/tenants/{tenant}/rooms/{room}/stream` (server-sent events).
Any member of the tenant may post to and read its rooms.

## The event

The chassis routes every room message to the tenant's `_room/0`; no hostname binding
is involved. A tenant with no `_room` stack gets no reply, as an unrouted request
gets a 404. The rule sees:

| Field | What |
|---|---|
| `@room.text` | The message |
| `@room.name` | The room |
| `@room.actor` | Who posted it |
| `@room.message_id` | The message's id |
| `@room.tenant` | The tenant |
| `@room.source.kind`, `@room.source.command` | Where it came from (`cli`, `thanks`) |

The reply is whatever the stack leaves at `.text`:

```txcl
# OPS/_room/100/echo.txcl
WHEN @src == "room"
  EMIT .text = &concat("echo: ", @room.text)
```

Replace the echo with real work: a model, a lookup, a hand-off to a person.

## The feed and its history

Both the message and the reply are published to the room's live feed, so everyone
watching sees them as they happen. The feed keeps a short backfill of recent
messages for someone who just joined; the record of a room is its runs, in the
[trace](../trace.md) like any other event (`txco trace last`).

On one node the feed is in process. A fleet needs a relay so a message posted to one
node reaches watchers on another: `--room-relay` names one a build registers. Open
core ships none, so the default (empty) is single-node.

## Example

[`examples/room-hello`](../../../examples/room-hello) — the echo stack above, ready to
`txco apply`.
