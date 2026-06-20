# room-hello

The smallest possible **room** — a durable shared context you talk to with
`thanks` (or `txco room`). A message you send becomes a normal event
(`@src == "room"`), routed to this workspace's `_room` stack, which here just
echoes it back. Replace the echo with real work — an AI op, a lookup, a human
handoff. A room message is just an event.

## Run it

Against a running chassis (e.g. `txco dev` in this directory):

```sh
txco apply

# one-shot send (prints the reply)
thanks --room support "hello"
txco room --room support "hello"      # same thing, explicit

# interactive: live feed + input (Ctrl-D to leave)
thanks --room support
```

`thanks` is the same binary as `txco`, installed under a second name; `thanks
<args>` is exactly `txco room <args>`.

## What's inside

| File | What |
|------|------|
| `OPS/_room/100/echo.txcl` | `WHEN @src == "room"` → `EMIT .text = &concat("echo: ", @room.text)` |
| `txco.yaml` | the `dev` target (local chassis) |

The chassis routes every `@src == "room"` event for the tenant into `_room/0`
(no hostname binding needed), runs this stack, and returns the `.text` reply.
Both your message and the reply appear in the room's live feed — open it with
`thanks --room support` (no message) and watch messages arrive as they happen.
Every room message is a normal traced event, so `txco trace last` shows it
routing `boot → detect-tenant → _room`.
