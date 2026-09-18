# TCP — line-delimited JSON

_The TCP head (`--tcp-listen-addrs`, default `:5050`) is the raw
socket channel: one JSON object per line in, one response line out,
connection held open. A listener can speak another protocol instead —
see [Protocol handlers](#protocol-handlers)._

## Wire protocol

- **In:** newline-delimited messages, up to 10 MB per line (an
  over-limit line is consumed and dropped; the connection stays open).
  Each line becomes one event.
- **Out:** by default, the merged envelope as one JSON line
  (`_`-prefixed fields stripped). A rule can write raw bytes instead
  via `@tcp.res.write` (base64-decoded on the way out), or close the
  connection with `@tcp.res.action = "close"` (after any write).
- On accept, a **connect event** fires first (before any line). Accept
  is implicit: the connection stays open unless the run was denied,
  failed, or set `@tcp.res.action = "close"`. A `@tcp.res.write` on the
  connect run is the greeting; the envelope itself is never echoed
  there. Then the read loop: line → message event → response, repeated
  until idle timeout or close.
- Admission denials are written as a `"<status> <reason>"` line, then
  the connection closes. A draining or full node answers a new
  connection with `503 draining` / `503 too many connections` and
  closes before any connect run.

The verdict subtree follows the same pattern as the other protocol
heads: `@tcp.*` holds facts the chassis observed (read-only),
`@tcp.res.*` is what the stack decides.

## Envelope fields

| Field | Meaning |
|---|---|
| `@tcp.listener` | Listener name (`--tcp-listen-addrs=webhooks=:5050,iot=:5051`; bare addresses are `default`) |
| `@tcp.local.{ip,port}` | The address the client connected *to* — route on raw port without any ingress config |
| `@tcp.remote.port` | The client's port (`@client.ip` is its address) |
| `@client.ip` | Client address — behind a trusted edge, the real client's, not the edge's |
| `@client.body` | The incoming line, base64-encoded (absent on the connect event) |
| `@tcp.res.write` | Verdict: base64 bytes to write to the socket |
| `@tcp.res.action` | Verdict: `"close"` hangs up after any write |
| `@tcp.inlet` | The inlet a hostname routes this listener's connections into: `_tcp`, or `_NAME` under `;handler=NAME` |
| `@tcp.host` | The connection's canonical hostname (TLS SNI, lower-cased, port and trailing dot dropped) — the routing fact detect-tenant reads |
| `@tcp.tls.enabled` | Whether the client's connection is TLS |
| `@tcp.tls.{sni,alpn,version}` | What the handshake observed (`sni` is the raw name; `host` above is its canonical form) |

The `@tcp.host` / `@tcp.tls.*` facts read the same whether the chassis
terminated TLS itself (`;tls`) or a trusted edge did and reported it
(`;proxy=`, below). A stack never needs to know which.

## Routing

A connection is routed once, on the connect event, through the same
`_sys/boot` path as every other source (`txco://detect-tenant`):

1. `@tcp.host` (the SNI — ours, or the one a trusted edge reported) is
   matched against the tenant's **verified** hostnames — the same gate a
   certificate needs, whatever `--require-hostname-verification` says for
   HTTP — and the connection enters that hostname's stack's **`_tcp`
   inlet**: hostname → stack `shop` → `shop/_tcp`. (On a listener with a
   [protocol handler](#protocol-handlers) it is that protocol's inlet
   instead: `shop/_echo`.)
2. Otherwise the listener name is matched against `ingress.tcp.listeners`
   in the [routing YAML](../../routing.md) — the single-destination case,
   where the operator names the stack outright.
3. Otherwise the connection is **closed** before any line is read. An
   unknown hostname never lands in a default tenant.

The connection is then **pinned**: every later event on it carries the
route the connect run chose, so it lands in the same stack without being
routed again. Pinning does not outlive the opt-in, though — before each
event the head asks the router whether that route still stands, and if it
no longer does (the inlet deactivated, the hostname revoked or re-bound)
the connection closes without running the event. Admission (a suspended
tenant, a rate limit) still applies to every event.

### Opting in: the `_tcp` inlet

TCP is **opt-in per stack**. A verified hostname that serves a stack over
HTTP is not a TCP endpoint until that stack has an active `_tcp` inlet —
a nested stack beside its other channels, like `_mail`:

```text
OPS/shop/0100_…            ← the stack's HTTP rules: never see a TCP event
OPS/shop/_tcp/0100_GREET/greet.txcl
```

```txcl
# OPS/shop/_tcp/0100_GREET/greet.txcl — the connect run (no line yet)
WHEN @client.body == ""
  EMIT @tcp.res.write = "aGVsbG8K"
```

```sh
txco push shop/_tcp        # a nested inlet is its own stack: push it by name
```

No `_tcp` inlet → step 1 misses and the connection is closed without a
single rule running, so opening a TCP port never exposes the stacks that
did not ask for it. `txco deactivate shop/_tcp` closes the door again:
new connections are refused, and open ones close at their next event. The
inlet shares its parent's hostname, and everything in it is
`@src == "tcp"` by construction — no source guards needed.

## TLS

```text
--tcp-listen-addrs "irc=:6697;tls,raw=:5050"
```

`;tls` terminates TLS on that listener with the bundled cert manager (the
same certificates the web and IMAPS heads serve; needs `--web-tls-addr`
or `--imap-tls-addrs` with the `dns` personality to exist). The
handshake's SNI becomes `@tcp.host`. For local development `;self-signed`
mints a dev certificate (loopback + `*.local.thanks.computer`, written
beside the data dir) instead — never for a public deployment.

```sh
openssl s_client -connect 127.0.0.1:6697 -servername irc.moo.local.thanks.computer
```

## Behind a TLS-terminating edge

```text
--tcp-listen-addrs "edge=:16697;proxy=172.16.0.0/12|fdaa::/8"
```

When an edge proxy terminates TLS in front of the chassis, the chassis
never sees the handshake — so the edge says what it saw. The contract is
one thing: a **PROXY protocol v2** header ahead of the stream, carrying

| Header part | Becomes |
|---|---|
| source / destination address | `@client.ip`, `@tcp.remote.port`, `@tcp.local.{ip,port}` |
| `PP2_TYPE_AUTHORITY` (the SNI) | `@tcp.tls.sni`, and canonicalised, `@tcp.host` |
| `PP2_TYPE_ALPN` | `@tcp.tls.alpn` |
| `PP2_TYPE_SSL` (+ `SSL_VERSION`) | `@tcp.tls.enabled`, `@tcp.tls.version` |

`;proxy=` lists the networks the edge connects from (`|`-separated CIDRs
or IPs — never spaces or commas inside an entry: both separate *entries*,
on the flag and in `TXCO_TCP_LISTEN_ADDRS`). The listener is an **edge-only door**: the header is required
from those networks, and a connection from anywhere else — or one that
skips or stalls the header — is closed before any event runs. A header an
outsider sends is never parsed. Keep the port off the public internet
anyway; the CIDR list is the trust boundary, not a substitute for one.

`;proxy=` does not combine with `;tls`: a listener either observes the
handshake or trusts an edge that did. Any proxy that writes this header
works (HAProxy's `send-proxy-v2-ssl` + `proxy-v2-options authority`
does); for Caddy, whose layer4 proxy sends no TLVs, the
[`caddy/proxytlv`](https://github.com/loremlabs/thanks-computer/tree/main/caddy/proxytlv)
module adds them.

## Protocol handlers

```text
--tcp-listen-addrs "raw=:5050,echo=:7007;tls;handler=echo"
```

Everything above — accept, caps, TLS or the edge header, the connect run,
routing, drain — is the head's, whatever is spoken afterwards. What
happens to the bytes *after* a connection is accepted belongs to the
listener's **handler**. `;handler=NAME` picks it; without the option a
listener speaks `line`, the protocol this page describes.

| Handler | Speaks | Inlet | Events |
|---|---|---|---|
| `line` (default) | one event per newline-terminated line | `<stack>/_tcp` | `@src == "tcp"` |
| `echo` | raw bytes written straight back; hangs up after 500 | `<stack>/_echo` | one `@src == "echo"` when the connection ends |

The handler's name does three jobs, so that a port's protocol is the
operator's choice and speaking it is each stack's:

- it is the **inlet** a hostname routes into — `_NAME` (`_tcp` for
  `line`). A stack opts into a protocol, not into "TCP": `shop/_tcp` does
  not make `shop` an echo endpoint;
- it is the **`@src`** of the events the handler emits, with the
  handler's facts under `@NAME.*`. The connection's `@tcp.*` facts and
  `@client.ip` ride along on every one of them;
- an unknown name refuses to boot — a typo never falls back to `line`.

The **connect run is the same on every listener**: a `@src == "tcp"`
event in the handler's inlet, answered on `@tcp.res.*`. So any protocol
can be greeted or refused in txcl before the handler reads a byte.

`echo` is the worked example — useful on its own for checking a listener,
an edge and a hostname end to end:

```txcl
# OPS/shop/_echo/0100_GREET/greet.txcl — the connect run
WHEN @src == "tcp"
  EMIT @tcp.res.write = "ZWNobyByZWFkeQo="

# OPS/shop/_echo/0200_CLOSED/closed.txcl — after the connection ended
WHEN @src == "echo"
  EMIT .echoed = @echo.bytes       # @echo.reason: limit | eof | idle | error
```

```sh
txco push shop/_echo
openssl s_client -quiet -connect 127.0.0.1:7007 -servername shop.local.thanks.computer
```

### Writing a handler

A handler is Go, compiled into the chassis, registered at init:

```go
func init() { tcp.RegisterHandler("irc", func(pu *processor.Unit) tcp.Handler { return &ircHandler{} }) }

func (h *ircHandler) ServeConn(ctx context.Context, rc tcp.RoutedConn) error {
    // rc.Conn is the decrypted stream; rc.Tenant / rc.Stack / rc.Host are trusted.
    res, err := rc.Emit(ctx, tcp.Event{Body: line, Facts: map[string]any{"cmd": "PRIVMSG"}})
    if err != nil {
        return err // rerouted, denied (*tcp.DeniedError), timed out, shutting down: hang up
    }
    // read the stack's verdict from res.Payload — @irc.res.*
    return nil
}
```

- `RoutedConn` carries only what the head observed or the router decided.
  A handler never learns a tenant from an envelope, or from the client.
- `Emit` is the only way to run an event. It stamps the source, the
  connection facts and the pinned route itself — after the handler's
  facts, so a handler cannot displace them — re-checks the route, and
  returns the run's result. Any error ends the connection.
- One handler value serves all of a listener's connections concurrently.
  A panic in `ServeConn` costs that connection, not the chassis.
- The stack's verdict for a new source needs its own author-writable
  subtree (`@irc.res.*`), allow-listed alongside `tcp.res` when the
  handler lands. Names another head owns (`mail`, `http`, `cron`, …) are
  refused at registration.
- There is no per-connection budget yet: each event is bounded like any
  other run, a connection's lifetime total is not.

## Timeouts and limits

| Flag | Default | Guards |
|---|---|---|
| `--tcp-connect-resp-timeout` | `3s` | The connect event's rule response |
| `--tcp-resp-timeout` | `10s` | Each message event's rule response (and the write back) |
| `--tcp-max-idle-timeout` | `5s` | Silence between lines (`echo`: between reads) |
| `--tcp-max-conns` | `0` (unlimited) | Open connections on this node, all listeners |
| `--tcp-max-conns-per-tenant` | `0` (unlimited) | Open connections per tenant, counted once the connect run has routed |
| `--tcp-handshake-timeout` | `5s` | The TLS handshake on a `;tls` listener, or the PROXY header on a `;proxy=` one |
| `--tcp-drain-timeout` | `5s` | How long shutdown waits for open connections to unwind after closing them |
