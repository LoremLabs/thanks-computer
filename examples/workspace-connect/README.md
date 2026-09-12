# workspace-connect

A service running **inside** a workspace, reachable from a browser over one
WebSocket: the stack binds it by name, the chassis carries its bytes.

The stack accepts an upgrade on `/connect`, and the page's first message
runs `workspace://tools/connect WITH service = "echo"`, which binds a TCP
echo server the workspace is running to that session. After that the socket
**is** the service's socket — every frame is its bytes, in both directions,
and **the stack does not run per frame** — until the tab closes, the service
hangs up, or the lease's `max_duration` passes.

This is the second attached transport, beside
[`workspace-terminal`](../workspace-terminal)'s PTY, and the one a noVNC page
rides to a workspace's browser: swap the echo server for `x11vnc` and the
page for noVNC and nothing between them changes.

## Run it

The service table is the operator's. This example adds `echo` on the
workspace's loopback port 17777:

```sh
cd examples/workspace-connect
TXCO_WORKSPACE_SERVICES=echo=17777 txco dev --allow-local-workspace
```

Open the URL `txco dev` prints. Click **start the service** (an ordinary
`exec` that leaves the echo server listening), then **connect**, then type a
line: it comes straight back from the service. Try **connect** with a service
name the table does not know, and see the refusal name the ones it does.
Click **connect** before **start** and see `unavailable`.

## How it works

| Piece | What it does |
|---|---|
| `conn/100/upgrade.txcl` | accepts the WebSocket on `/connect` (a binding is a capability the stack grants here, once) |
| `conn/100/start.txcl` | `workspace://tools/exec` starts a tiny Python echo server on loopback 17777, detached from the exec, idle-exits after 10 min, no-op if already up |
| `conn/_websocket/100/parse.txcl` | decodes the first message `{"type":"connect","service":"echo"}` |
| `conn/_websocket/200/connect.txcl` | `workspace://tools/connect WITH service = .msg.service` binds the service and returns the lease |
| `conn/FILES/index.html` | a bytes console: typed lines → binary frames, echoed bytes → the log |

### A name, never a port

`service` is resolved against a table the chassis owns: `browser` is built in
(5900, a workspace's VNC display), and an operator adds more with
`--workspace-services name=port` (or `TXCO_WORKSPACE_SERVICES`). No `WITH`
value can carry a host or a port, so a stack — or a model whose output
reached a `WITH` — can choose a service the node knows about and nothing
else. That is what keeps `connect` an attachment rather than a tunnel.

### The wire protocol

- **Binary frames** are the service's bytes: client → service, service →
  client. No base64, no framing of ours.
- **Text frames** are the same control envelope `attach` uses:
  - client → server: `{"type":"detach"}`; `resize` and `signal` are answered
    with one `{"type":"error"}` frame — a byte stream has nothing to resize.
  - server → client: `{"type":"attached"}` the moment the binding is live
    (wait for it — a service may not speak first), then
    `{"type":"exit","code":0}` when the service hangs up,
    `{"type":"expired"}`, `{"type":"error","code":"…","error":"…"}` when the
    connect was refused or nothing was listening (`unavailable`).

### Metering

A binding is a metered lease, not a long request: it heartbeats every 60 s
at the ordinary workspace rate (1 fuel per 30 s), charged to the tenant, and
a lease that stops heartbeating (a crashed node) is reapable rather than
pinning the workspace forever.

## Smoke

`probe.json` boots the stack with the operator's service table and checks the
HTTP half (the page, the 426, `/start` leaving the service up, idempotently).
The WebSocket half is covered by Go tests in `chassis/processor`,
`chassis/attach` and the websocket personality.
