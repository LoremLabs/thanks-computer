# Protocols — every channel, one envelope

_How each wire protocol becomes (or consumes) the chassis's JSON
envelope. The conceptual story is [ingress](../../ingress.md); these
pages are the per-channel facts._

Whatever arrives, the head that received it stamps `_txc.src` and its
own namespace (`@web.req.*`, `@lmtp.*`, `@cron.*`, `@tcp.*`) onto one
envelope, and the same rules engine takes it from there. Outbound is
symmetric: rules dispatch out over HTTP, MCP, or SMTP.

| Channel | Direction | Page |
|---|---|---|
| HTTP | in (+ responses, streaming) | [web.md](./web.md) |
| Email — receiving | in | [lmtp.md](./lmtp.md) |
| Email — sending | out | [sendmail.md](./sendmail.md) |
| Cron | in (time as a channel) | [cron.md](./cron.md) |
| TCP | in (line-delimited JSON) | [tcp.md](./tcp.md) |
| MCP (agent tools) | out (in: see page) | [mcp.md](./mcp.md) |
| DNS | authoritative answers for delegated zones | [dns.md](./dns.md) |

Before any rule fires, the **router** decides which tenant and stack
an event belongs to — hostname bindings in the DB (live, no restart)
or a static YAML file: [routing.md](./routing.md).

Heads are enabled per chassis with `--personalities` (default
`cron,tcp,web,admin`; `lmtp` and `dns` are opt-in) — see the
[runtime reference](../serve.md).
