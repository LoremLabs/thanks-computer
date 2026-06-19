# Web — HTTP in, HTTP out

_The web head (`--web-addr`, default `:8080`) turns HTTP requests into
envelopes and the merged flow back into responses._

## Request → envelope

| Envelope field | From |
|---|---|
| `@web.req.method` | HTTP verb |
| `@web.req.url.{full,scheme,path,hostname,port,query.<k>.0,query.raw}` | URL parts; query values are arrays |
| `@web.req.headers.<name>` | Headers (arrays) |
| `@web.req.cookies.<name>` | Parsed cookies |
| `@web.req.body` | Body, base64-encoded |
| `@web.req.host` / `@web.req.proto` | Host header / protocol |
| `@rid` | From `X-Request-ID`, or a fresh UUID |

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


## Envelope → response

Rules (or ops) shape the response by writing:

- `@web.res.status` — defaults to `200`
- `@web.res.headers.<name>` — response headers
- `@web.res.body` — base64; decoded and written raw
- `@halt` — `true` — stop execution of further steps.

With no `@web.res.body`, the response is the merged envelope as JSON,
with `_`-prefixed fields stripped (the privacy convention).

```json
{
  "_txc": {
    "halt": true,
    "web": {
      "res": {
        "body": b64"Thanks, Computer",
        "headers": {
          "content-type": [
            "text/plain; charset=utf-8"
          ]
        },
        "status": 200
      }
    }
  }
}
```

Which renders as:

```http
HTTP/1.1 200 OK
Content-Type: text/plain; charset=utf-8
Server-Timing: total;dur=10
X-Request-Id: CcAw2fqr8j3PmWf5eG9qP
X-Txco-Served-By: imac; sid=CcAvUzHFQWxDcsc44kADw
Date: Thu, 18 Jun 2026 15:05:54 GMT
Content-Length: 17

Thanks, Computer
```

**Streaming:** setting `@web.res.body` in a *non-terminal* scope locks
the status + headers and switches to chunked transfer — each
subsequent chunk flushes immediately, with natural backpressure (the
processor blocks until the flush completes). HEAD requests and
1xx/204/304 statuses omit body bytes.

## Limits and timeouts

- Headers capped at 256 KiB.
- The per-request deadline derives from `--op-timeout-max` (10m) —
  deliberately separate from `--web-write-timeout` (15s), which guards
  header reads (slowloris), so long-running ops aren't cut off by the
  socket timeout.

## TLS

Plain HTTP by default — a front proxy terminates TLS. With
`--web-tls-addr` (e.g. `:8443`) the chassis terminates TLS itself,
serving certificates per SNI from its cert manager (ACME DNS-01;
requires the `dns` personality and `--acme-email`).

## Special request signals

| Signal | Effect |
|---|---|
| `?_txc.continuation=<id>` | Poll a suspended run ([continuations](../../continuations.md)) |
| `?_txc.break=N` or `=stack/N` | Breakpoint gate — only with `--debug-breakpoints` |
| `X-Txco-Mocks: <patterns>` | Request-scoped [mock](../../authoring/mocks.md) patterns — only with `--web-mock-header` |
| `_txc.admission.*` (set by rules) | Denials translate to HTTP status + headers before normal render |
