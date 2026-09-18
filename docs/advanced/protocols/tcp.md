# TCP — line-delimited JSON

_The TCP head (`--tcp-listen-addrs`, default `:5050`) is the raw
socket channel: one JSON object per line in, one response line out,
connection held open._

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
| `@tcp.remote.port` | The peer's port (`@client.ip` is its address) |
| `@client.ip` | Peer address |
| `@client.body` | The incoming line, base64-encoded (absent on the connect event) |
| `@tcp.res.write` | Verdict: base64 bytes to write to the socket |
| `@tcp.res.action` | Verdict: `"close"` hangs up after any write |

| `@tcp.host` | The connection's canonical hostname (TLS SNI, lower-cased, port and trailing dot dropped) — the routing fact detect-tenant reads |
| `@tcp.tls.enabled` | Whether the listener terminated TLS |
| `@tcp.tls.{sni,alpn,version}` | What the handshake observed (`sni` is the raw name; `host` above is its canonical form) |

## Routing

A connection is routed once, on the connect event, through the same
`_sys/boot` path as every other source:

1. `@tcp.host` (from SNI) is matched against the tenant's **verified**
   hostnames — the same gate a certificate needs, whatever
   `--require-hostname-verification` says for HTTP.
2. Otherwise the listener name is matched against `ingress.tcp.listeners`
   in the [routing YAML](../../routing.md) — the single-destination case.
3. Otherwise the connection is **closed** before any line is read. An
   unknown hostname never lands in a default tenant.

Every later line on the connection is an event in the tenant the connect
run chose.

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

## Timeouts and limits

| Flag | Default | Guards |
|---|---|---|
| `--tcp-connect-resp-timeout` | `3s` | The connect event's rule response |
| `--tcp-resp-timeout` | `10s` | Each message event's rule response (and the write back) |
| `--tcp-max-idle-timeout` | `5s` | Silence between lines |
| `--tcp-max-conns` | `0` (unlimited) | Open connections on this node, all listeners |
| `--tcp-max-conns-per-tenant` | `0` (unlimited) | Open connections per tenant, counted once the connect run has routed |
| `--tcp-handshake-timeout` | `5s` | The TLS handshake on a `;tls` listener |
| `--tcp-drain-timeout` | `5s` | How long shutdown waits for open connections to unwind after closing them |
