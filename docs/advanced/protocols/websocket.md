# WebSocket — a live session in the browser

_The `websocket` personality turns a WebSocket connection into a chassis-owned
session: the upgrade is an ordinary web request a stack accepts, every message
the client sends is one bounded run, and the stack answers through
`txco://websocket/*` ops. The socket never enters processor execution; an idle
connection runs nothing._

Enable it with `websocket` in `--personalities` alongside `web` (`txco dev`
has it on). There is no listener of its own: sessions arrive through the web
head's Upgrade handoff.

## Upgrade → accept

A request carrying `Upgrade: websocket` runs through the stack's web ops like
any other, with two extra chassis-stamped facts:

| Envelope field | Meaning |
|---|---|
| `@websocket.upgrade` | `true` — this request asks to upgrade |
| `@websocket.session.id` | the id the session will have if accepted (`ws_…`, minted, unguessable) |
| `@websocket.subprotocols` | what the client offered in `Sec-WebSocket-Protocol`, if anything |

Nothing upgrades until a rule EXECs **`txco://websocket/accept`**. Which
paths become sessions, for whom, with which Origin policy — all of that is the
stack's WHEN clause:

```txcl
WHEN @websocket.upgrade == true
  && @web.req.url.path == "/chat"
  && ._sess.email != ""
  EXEC "txco://websocket/accept"
  WITH state.email = ._sess.email,
       events = &array("close")
```

| `accept` WITH | |
|---|---|
| `state` | an object (≤ 4 KiB) stamped verbatim on every message as `@websocket.session.state` — the already-resolved principal, a room, whatever the session should carry; null members are dropped |
| `origins` | host patterns allowed cross-origin (`&array("app.example")`); `"*"` disables the check. Default: same-host, or no `Origin` header (a non-browser client) |
| `subprotocols` | what the stack will speak, in preference order; the negotiated one is `@websocket.session.subprotocol` |
| `subprotocol_required` | `true` refuses the upgrade (400) when the client offers none of them |
| `events` | extra runs beyond messages: `&array("close")` |
| `idle_timeout` | per-session, up to `--websocket-max-idle-timeout` |
| `max_message_bytes` | per-session, up to `--websocket-max-message-bytes` |
| `into` | output key, default `_websocket` |

Output: `_websocket.accepted = true`, `_websocket.session.id`. When the run
ends the chassis completes the handshake (`101`). An upgrade request with no
accept renders as ordinary HTTP — the stack's `401`/`404`/`426`, or the default
projection — so a refused upgrade is a normal HTTP error response, never a
half-open socket. The chassis refuses on its own only for the connection caps
and drain (`503` + `Retry-After`) and for a bad handshake or Origin (`400`/`403`).

A stack may accept one path, several, a regex, or all; every session of a
stack enters the same `<stack>/_websocket/0`, and the accepting path
(`@websocket.req.path`) plus the accept-time `state` are how the `_websocket`
ops tell endpoints apart.

## Message → envelope

Each complete inbound message (fragments are reassembled) is **one run** of
`<stack>/_websocket/0`, strictly in order per session — the next message
waits for this run to finish, so per-session state never races. The route is
pre-stamped, so the boot pipeline promotes it straight to the sub-stack.

| Envelope field | From |
|---|---|
| `@src` | `websocket` |
| `@websocket.phase` | `message`, or `close` for the opt-in close run |
| `@websocket.session.{id, stack, subprotocol, connected_at, seq}` | the session; `seq` counts its runs |
| `@websocket.session.state` | the accept op's `state`, verbatim |
| `@websocket.req.{host, path, origin, user_agent}` | the upgrade request, snapshotted |
| `@websocket.msg.type` | `text` or `binary` |
| `@websocket.msg.text` / `@websocket.msg.data` | the payload — text inline, binary base64 (`data`) |
| `@websocket.msg.bytes` | payload size |
| `@websocket.close.{code, reason, initiated_by}` | close runs only; `initiated_by` is `client`, `stack`, or `chassis` |
| `@websocket.tenant` | tenant slug |
| `@client.ip` | first `X-Forwarded-For` hop, else the peer |

```json
{
  "_ts": "2026-09-04T12:00:00Z",
  "_txc": {
    "src": "websocket",
    "rid": "CcAvW7aoT26xqmjgGVZbw",
    "client": { "ip": "203.0.113.7" },
    "route": { "tenant": "acme", "stack": "counter/_websocket", "to": "counter/_websocket/0",
               "ingress": "host:counter.local.thanks.computer", "hostname_verified": true },
    "websocket": {
      "tenant": "acme",
      "phase": "message",
      "session": { "id": "ws_01K4BZ…", "stack": "counter", "subprotocol": "",
                   "connected_at": "2026-09-04T11:59:00Z", "seq": 3,
                   "state": { "email": "a@b.c" } },
      "req": { "host": "counter.local.thanks.computer", "path": "/ws",
               "origin": "https://counter.local.thanks.computer", "user_agent": "Mozilla/5.0 …" },
      "msg": { "type": "text", "text": "{\"type\":\"increment\"}", "bytes": 20 }
    }
  }
}
```

Every `@websocket.*` fact is chassis-stamped and read-only for rules; a stack
may `EMIT @delete = &array("@websocket.msg.text")` (or `.data`) once it has
consumed a large payload, so it stays out of the trace and any continuation.
Parse text with the ordinary builtins: `EMIT .msg = &json(@websocket.msg.text)`.

**KV namespace:** these runs' stack is `<stack>/_websocket`, and `txco://kv/*`
defaults to the **app stack's** namespace (`<stack>`) — a `_`-nested inlet
sub-stack shares its app's state, the same rule `<stack>/_mail` follows — so a
web request and a session see the same keys. `WITH namespace` still overrides.

## Stack → client

A stack talks back with ops — there is no `@websocket.res`. Ops work mid-run
(stream a long answer as several frames), from other runs (a cron pushes to
a session id it stored in KV), and after a continuation resumes.

| Op | WITH | Output |
|---|---|---|
| `txco://websocket/send` | `session_id`; `text` (a string as-is, any other value as JSON text) **or** `data` (base64 → binary frame) | `_websocket.sent.{session_id, bytes, type}` |
| `txco://websocket/reply` | as `send` without `session_id` — the session this envelope came from; only inside a websocket run (or its resumed continuation) | same |
| `txco://websocket/close` | `session_id` (implicit inside a session run), `code` (default 1000; 1000, 1001, 1003, 1007–1011, 3000–4999), `reason` (≤ 123 bytes) | `_websocket.closed.{session_id, code}` |

```txcl
WHEN .msg.type == "increment"
  EXEC "txco://websocket/reply"
  WITH text.count = ._kv          # sent as {"count": 42}
```

Errors land at `<into>.error.{code, message}` and the run continues:
`txco_websocket_session_not_found` (no live session with that id for this
tenant on this node — a closed session, a wrong id, or another tenant's; the
three are indistinguishable), `_session_closed`, `_write_timeout` (the client
did not read within `--websocket-write-timeout`; the session was closed),
`_message_too_large`, `_not_session_run` (`reply` outside a session run),
`_not_upgrade` (`accept` outside an upgrade run), `_invalid_close_code`,
`_bad_argument`, `_state_too_large`, `_disabled` (the personality is off on
this node — every op is registered regardless, so a stack can branch).

A WITH path that resolves to nothing arrives as `null`: `text = .missing` is
`txco_websocket_bad_argument`, never an empty frame.

Sending to a closed session errors; nothing reconnects on the stack's behalf.
A reconnecting client is a new session with a new id — continuity across
reconnects is the application's, through the authenticated identity in
`state` or a key the client presents (doc: "WebSocket is live transport;
durability is the continuation store / pub-sub").

## Limits and timeouts

| Flag | Default | |
|---|---|---|
| `--websocket-max-conns` | 8192 | open sessions per node; past it an upgrade is `503` + `Retry-After`. A memory guard (an idle session is ~50 KB and no processor time), not a throughput limit; keep it under the process's file-descriptor limit and any front proxy's connection cap |
| `--websocket-max-conns-per-tenant` | 2048 | per tenant per node, same refusal; stops one tenant taking a node's whole budget |
| `--websocket-max-message-bytes` | 256 KiB | one complete message, in or out; a larger inbound one closes the session with `1009` |
| `--websocket-inbound-queue` | 16 | messages waiting behind the running one; a full queue closes with `1013` rather than growing memory |
| `--websocket-run-timeout` | 60s | the run one message makes; a late result is discarded, the session stays |
| `--websocket-idle-timeout` | 5m | no application message either way → `1000 idle timeout` (ping/pong does not count); per-session up to `--websocket-max-idle-timeout` (1h) |
| `--websocket-ping-interval` | 25s | chassis liveness pings; a missing pong closes with `1011`. Under fly-proxy's ~60s HTTP idle on the Flycast hop |
| `--websocket-write-timeout` | 10s | one `send`/`reply` write; a peer that will not read is closed (`1011`) — messages are never silently dropped |
| `--websocket-drain-timeout` | 5s | shutdown: every session gets `1001 going away`; this bounds the wait |

**Admission** runs per message like any run (the `_sys` → tenant pin fires
once per run): a `429` (rate limit, concurrency) drops that message and keeps
the socket; `402`/`403` close with `1008`; `503` closes with `1013`. A
suspended tenant's upgrade request is denied before its accept op can run.

**Backpressure, as built:** inbound is the bounded queue above; outbound is
one bounded write per `send` — the library serializes concurrent writers, so
a slow client stalls senders for at most the write timeout, then loses the
session. No outbound queue in v1, by choice: correctness over silent drops
with far less machinery.

**Fuel:** connection lifetime is not processor lifetime. An open, idle socket
meters nothing; each message run meters as a run, each op as an op; ping/pong
never reaches a stack. Outbound bytes are not metered separately in v1.

**Continuations:** a message run that suspends is "finished" from the
session's point of view; when it resumes, `txco://websocket/reply` still finds
the session if it is alive on this node (the resume re-pins the source), and
`send` with `session_id = @websocket.session.id` is the belt-and-braces
spelling.

**Streaming:** `@web.res.body` written from a websocket run is drained and
ignored — the socket is the transport; use `reply`, as many times as needed.

## Deployment

- Sends resolve on **this node** in v1: a session lives where its socket is,
  `reply` always works (the run executes there), and a `send` from another
  node answers `session_not_found`. A fleet session directory is the next
  step behind the same registry seam.
- Fronting proxies must pass Upgrade and keep the stream: on the hosted edge,
  Caddy's `reverse_proxy` does by default, with `stream_close_delay` set so an
  edge reload drains sessions instead of severing them; keep `stream_timeout`
  unset. Fly's connection-based concurrency counts each session as a
  connection — set the limits accordingly.
- Hostname classes behind Cloudflare's proxy inherit its ~100s idle window;
  `*.stacks.thanks.computer` and custom domains are gray-cloud (direct to the
  edge), so the chassis pings alone hold the connection.
- A chassis terminating TLS itself (`--web-tls-addr`) negotiates HTTP/2;
  browsers open an HTTP/1.1 connection for `wss://` so this works, but an h2
  client cannot upgrade (RFC 8441 is not implemented).
- The access log records an upgrade as `status=101 size=0`; every session
  logs one `websocket open` and one `websocket close` line (counts, duration,
  code, who closed) and never a payload. Metrics:
  `chassis.websocket.{upgrades,messages,closes,connections}`.

See [`examples/websocket-counter`](../../../examples/websocket-counter) for the
whole shape in eight small ops.
