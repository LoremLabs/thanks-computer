# websocket-counter

The smallest possible **live session**: a browser opens one WebSocket, and
every message it sends becomes a normal, bounded run of a stack — while the
socket stays open, owned by the chassis, running nothing in between.

## Run it

Against a running chassis (`txco dev` in this directory; dev has the
`websocket` personality on by default):

```sh
txco apply
```

Open `http://<the counter hostname txco dev printed>/index.html` and click
**increment**: the page sends `{"type":"increment"}` and shows the
`{"count": N}` that comes back on the same connection. **send junk** shows the
error frame; **close** ends the session and runs the close event.

By hand, with [websocat](https://github.com/vi/websocat):

```sh
websocat ws://localhost:8080/ws -H 'Host: <that hostname>'
{"type":"increment"}
{"count":1}
{"type":"increment"}
{"count":2}
```

`GET /ws` without upgrade headers answers the stack's own `426`.

## What's inside

| File | What |
|------|------|
| `OPS/counter/100/upgrade.txcl` | `WHEN @websocket.upgrade == true && @web.req.url.path == "/ws"` → `EXEC "txco://websocket/accept" WITH events = &array("close")` — the stack's decision to take the connection (auth, Origin, subprotocols go here) |
| `OPS/counter/100/not_upgrade.txcl` | `/ws` as plain HTTP → `426 Upgrade Required` |
| `OPS/counter/100/home.txcl` | `/` → `/index.html` |
| `OPS/counter/FILES/index.html` | the browser client |
| `OPS/counter/_websocket/100/parse.txcl` | `EMIT .msg = &json(@websocket.msg.text)` |
| `OPS/counter/_websocket/200/increment.txcl` | `txco://kv/incr` on `count:<session id>` |
| `OPS/counter/_websocket/300/reply.txcl` | `txco://websocket/reply WITH text.count = ._kv` → `{"count": N}` |
| `OPS/counter/_websocket/300/unknown.txcl` | anything else → an error frame (the socket stays open) |
| `OPS/counter/_websocket/900/closed.txcl` | the opt-in close event → `txco://kv/delete` |

## How it flows

1. The browser's `GET /ws` (with `Upgrade: websocket`) is an ordinary web
   request: the chassis stamps `@websocket.upgrade = true` and a minted
   `@websocket.session.id`, then runs `counter/0…` like any request.
2. `100/upgrade.txcl` fires and EXECs `txco://websocket/accept`. Any path,
   any number of paths, any condition — the WHEN is the policy. Without an
   accept the request renders as normal HTTP (here, the 426).
3. The chassis completes the handshake (`101`) and owns the socket from
   here: reads, ping/pong, limits, idle, shutdown.
4. Each complete message the browser sends is **one run** of
   `counter/_websocket/0` with `@src == "websocket"`, the message on
   `@websocket.msg.text`, and the session facts on `@websocket.session.*`.
   Runs are serialized per session, so `count` never races.
5. `txco://websocket/reply` writes back on that socket; `send` does the
   same for any `session_id` from any run; `close` ends one.
6. When the session ends, the opt-in close run cleans up.

Every message run is a normal traced, metered event — `txco trace last`
shows it. Garbage in (`not json`) fails the parse EMIT, `.msg` stays
absent, and `300/unknown.txcl` answers; the socket stays open.

See [docs/advanced/protocols/websocket.md](../../docs/advanced/protocols/websocket.md)
for the envelope, the ops, and the limits.
