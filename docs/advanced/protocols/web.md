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

## Envelope → response

Rules (or ops) shape the response by writing:

- `@web.res.status` — defaults to `200`
- `@web.res.headers.<name>` — response headers
- `@web.res.body` — base64; decoded and written raw

With no `@web.res.body`, the response is the merged envelope as JSON,
with `_`-prefixed fields stripped (the privacy convention).

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
