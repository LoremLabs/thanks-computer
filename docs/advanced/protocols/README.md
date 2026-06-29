# Protocol Support

Multiple protocols are supported in the [Thanks, Computer](https://www.thanks.computer) chassis. They all become `JSON`.

## Protocols as JSON

We call the processes that speak the protocol a `head` in the `txco` chassis. These
ingress points speak the native protocol and then serialize it to a `JSON` event.
 
```json
{
  "_ts": "2026-06-18T16:58:59+02:00",
  "_txc": {
    "_seen": [
      "boot/0->boot/50",
      "boot/50->boot/100"
    ],
    "flag_breakpoint": true,
    "fuel_used": 105,
    "hostname_verified": true,
    "ingress": "host:build-1.local.thanks.computer",
    "rid": "CcAvW7aoT26xqmjgGVZbw",
    "src": "http",
    "stack": "build-1",
    "tenant": "default",
    "ttl": 497,
    "web": {
      "req": {
        "headers": {
          "Accept-Encoding": [
            "gzip"
          ],
          "User-Agent": [
            "Go-http-client/1.1"
          ]
        },
        "host": "build-1.local.thanks.computer",
        "method": "GET",
        "proto": "HTTP/1.1",
        "url": {
          "full": "/",
          "path": "/"
        }
      }
    }
  }
}
```

When these events arrive, the head that received it stamps `_txc.src` and its
own namespace (`@web.req.*`, `@lmtp.*`, `@cron.*`, `@tcp.*`) onto one flow
envelope, and the same rules engine takes it from there. 

Each of these protocol heads also know how to convert back from the JSON event used
in an opstack's flows into the protocol.

## Protocols supported

| Channel | Direction |
|---|---|
| [HTTP](./web.md) | bidirectional, with streaming |
| [Email — receiving](./lmtp.md) | in |
| [Email — sending](./sendmail.md) | out |
| [Cron](./cron.md) | in |
| [Scheduled](./scheduled.md) | in, time shifted — `txco://schedule` enqueues, fires later into `_scheduled` |
| [TCP](./tcp.md) | bidirectional |
| [MCP](./mcp.md) (agent tools) | out, in as diy |
| [DNS](./dns.md) | not a stack input. authoritative answers for delegated zones. |

