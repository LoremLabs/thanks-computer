# TCP — line-delimited JSON

_The TCP head (`--tcp-listen-addrs`, default `:5050`) is the raw
socket channel: one JSON object per line in, one response line out,
connection held open._

## Wire protocol

- **In:** newline-delimited messages, up to 10 MB per line (over-limit
  lines are dropped). Each line becomes one event.
- **Out:** by default, the merged envelope as one JSON line
  (`_`-prefixed fields stripped). A rule can write raw bytes instead
  via `@server.write` (base64-decoded on the way out), or close the
  connection with `@server.hangup = true`.
- On accept, a **connect event** fires first (before any line) — rules
  can greet, gate by IP, or hang up. Then the read loop: line →
  message event → response, repeated until idle timeout or hangup.
- Admission denials are written as a `"<status> <reason>"` line, then
  the connection closes.

## Envelope fields

| Field | Meaning |
|---|---|
| `@tcp.listener` | Listener name (`--tcp-listen-addrs=webhooks=:5050,iot=:5051`; bare addresses are `default`) |
| `@tcp.local.{ip,port}` | The address the client connected *to* — route on raw port without any ingress config |
| `@client.ip` | Peer address |
| `@client.body` | The incoming line, base64-encoded |

Named listeners are also [routing keys](../../routing.md) mapping a listener name to a tenant/stack.

## Timeouts

| Flag | Default | Guards |
|---|---|---|
| `--tcp-connect-resp-timeout` | `3s` | The connect event's rule response |
| `--tcp-resp-timeout` | `10s` | Each message event's rule response |
| `--tcp-max-idle-timeout` | `5s` | Silence between lines |
