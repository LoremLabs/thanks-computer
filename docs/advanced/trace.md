<!-- nav: Trace internals -->

# Trace logs

_For debugging what a flow actually did, after the fact — no rerun, no debug logging._

The chassis records per-request artifacts (the inbound envelope, every op execution, the final response, plus a timeline of routing events) to durable storage. Developers browse the result to see exactly what happened during a request without rerunning it or turning on debug logging.

## Modes

`--trace-mode` controls how much detail is written:

| Mode            | What gets written                                                                            |
| --------------- | -------------------------------------------------------------------------------------------- |
| `off` (default) | Nothing. Zero-cost no-op.                                                                    |
| `summary`       | Request header + timeline + per-step `meta.json` (timings, sizes, status). No payload bytes. |
| `full`          | Everything, including handler in/out bodies per step.                                        |

`--trace-dir` is the root (default `./data/trace`). `--trace-mode=full --trace-dir=/var/log/txco/traces` is a typical production setting when you actually want payloads. In `full` mode with `--trace-async`, bodies are capped per step by `--trace-body-cap-bytes` (default 65536); a synchronous sink writes them whole.

`--trace-async=true` buffers writes through a worker goroutine so the request path never blocks on disk I/O. Recommended in production.

## File layout

Per request, under `<trace_dir>/requests/<rid>/`:

```
in.json            inlet's initial envelope + chassis metadata (rid, src, tenant, stack)
out.json           chassis's final response, after all merges
timeline.jsonl     line-per-event log (request.start, step.start, step.end, request.end)
steps/
  0000-boot/       one folder per fired op, zero-padded scope so the directory sorts in scope order
    op.json        the rule's stored definition (txcl, exec, etc.)
    in.json        envelope handed to the handler (post-SELECT: for a rule that selects, this is the projected view — what the op actually received)
    out.json       handler's raw response
    meta.json      timing, sizes, status, transport
```

Two ops at the same scope produce sibling step folders with the same `NNNN-` prefix — that's the trace's signal for "ran in parallel."

In `summary` mode the per-step `op.json` / `in.json` / `out.json` are omitted; `meta.json` and the timeline still record what ran and how long it took.

## Browsing traces — `txco trace`

```sh
txco trace                  # list recent traces with rid, tenant, src, route, duration
txco trace <rid>            # one-row-per-step table for that request
txco trace last             # alias for the most recent rid
txco trace <rid> --verbose  # include payloads (full mode only)
```

Other flags: `--step`, `--json`, `--plain`, `--grep`, `--watch`.

The admin UI's Traces tab shows the same data with a step-by-step explorer.

## Keeping sensitive data out of traces

Trace logs persist whatever the rules touched, byte-for-byte. Credentials and raw mail are scrubbed by the chassis from the first record of every request ([below](#always-scrubbed)). For everything else a rule's own data can hold, such as classifier outputs or personal details, two reserved `WITH` keys on any rule scrub the trace artifacts:

```txcl
WHEN @web.req.url.path = "/checkout"
WITH redact = "_order.card_token, _order.email"
WITH omit   = "_classify.transcript"
EXEC "op://CHECKOUT"
```

| Keyword               | Effect on the trace JSON                                                                 |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `redact = "a.b, c.d"` | Replaces each path's value with the string `"[REDACTED]"`. Field stays; value is masked. |
| `omit = "x.y"`        | Deletes the path entirely. Field vanishes from the trace JSON.                           |

- Runtime behavior is unaffected — redaction changes only what hits durable storage. (What the EXEC receives is a separate mechanism: the rule's own `SELECT` projection.)
- **A hint starts when the request reaches its stack.** Every request begins in `_sys`, so the inbound record (`in.json`) and the boot steps are written before any tenant's hint applies. A hint therefore protects what your stack adds to the envelope. It cannot protect what arrived with the request, such as `_txc.web.req.body`.
- Paths are exact gjson dot-paths.
- A path listed in both `redact` and `omit` is resolved by **omit wins** (the more aggressive choice).
- Hints are **scoped per `(tenant, stack)`**: a `redact` declared in `acme/support` does NOT bleed into `acme/billing` even though both are tenant `acme`. If a request enters `acme/support` and an EXEC jumps to `acme/billing`, both stacks' hints apply to the trace from that point on (union semantic).
- Hints are **static** — only literal string values are honored. `WITH redact = @some.path` (runtime resolution) is silently skipped, since the trace sink must know the paths at write time.
- The chassis collects hints at startup and rebuilds the registry on every dbcache reload (so `txco apply` picks up new hint declarations without a restart).
- Application order is **omits first**, then redacts. Applying omits first lets `_txc.lmtp.msg` vanish without wasting a sentinel write on `_txc.lmtp.msg.headers.authorization`.

### Always scrubbed

Unless `--trace-no-scrub` is set, these are removed from every record of every request, from the first, whatever a stack declares:

| What | Paths | Effect |
| ---- | ----- | ------ |
| Credential headers, in any case: `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Auth-Token` | under `_txc.web.req.headers`, `_txc.web.res.headers`, `_txc.llm.headers` | value becomes `"[REDACTED]"` |
| Parsed cookies | `_txc.web.req.cookies` | value becomes `"[REDACTED]"` |
| Mail content | `_txc.lmtp.msg.raw`, `.html`, `.attachments` | field removed |

Rules still read all of these at runtime. Declaring a hint for one is harmless and no longer needed.

To debug auth or mail, `--trace-no-scrub` (`TXCO_TRACE_NO_SCRUB`) keeps these values in traces. Stack hints still apply, and a record that was cut short under a hint is still dropped. `txco dev` turns it on, so traces under `.txco/dev/trace` can hold whatever keys and cookies your dev chassis handles; `TXCO_TRACE_NO_SCRUB=false txco dev` shows what production records. The chassis logs a warning at boot when it is on. Traces already written stay as they are after you turn it off, so on a chassis that handles real credentials, rotate anything recorded while it was on.

### Where it runs

The redaction layer lives below the async-sink boundary, so in async mode the work happens on the trace worker goroutine — off the request hot path. Per-write cost is one `gjson.Get` existence check plus an `sjson.Set` or `sjson.Delete` per declared path; sub-millisecond for typical envelopes.

In sync mode (`--trace-async=false`) redaction runs on the request goroutine just before the disk write. Still cheap; the disk fsync that follows is the dominant cost.

A record over `--trace-body-cap-bytes` is cut in async mode, and JSON that has lost its end cannot be searched. So the always-scrubbed fields are removed from the whole record first, on the request goroutine; only an oversized record pays for this. A stack's own hints cannot be applied to a record that is still cut short, so in a stack that declares any, that record is replaced with a note saying it was dropped.

### A key a person pastes

A key pasted into a page arrives in a request's body, which is on the record before any hint applies and is not always scrubbed. To keep it out altogether, send the request with the `X-Txco-Pasted-Key` header instead. The web head keeps its body off the envelope entirely and `txco://connection/put` reads it from the request's context, so it reaches no rule, step or trace. The header only keeps a body from the rules: it grants nothing.

### A value read back

A secret a stack keeps and shows again, a stored password read to show its owner, is not issued by the request, so nothing scrubs it. A hint cannot do it either: a hint applies to its whole stack, so hiding the response body that shows the value would blank every response. Name the value on the rule that produces it:

```txcl
WHEN @web.req.url.path == "/api/password"
  WITH key = "password", into = "_pw", trace_secret = "_pw.password"
  EXEC "txco://kv/get"
```

`trace_secret` is one or more comma-separated paths into the op's own output. The value goes from every record of the request after that op, and from the op's own step: as itself, and as it reads inside base64 text. It works on a rule that makes a value by `EMIT` too. Values under 16 characters are left alone, because a short one would shred unrelated bytes.

### What's NOT redacted

- **Runtime data** is never touched. The rule's matchers, the op's (possibly SELECT-projected) input, the merged response all carry real, unmasked values.
- **Request bodies, query strings and mail text** (`_txc.web.req.body`, `_txc.web.req.url.query`, `_txc.lmtp.msg.text`) stay on the record, because debugging needs them. A password posted to a form is on the record: see [above](#a-key-a-person-pastes) for keeping a value off it.
- **A copy** a rule makes of a scrubbed value, to a path of its own, is not scrubbed. Name that path in a hint.
- **The rule's source** is recorded with each step (`op.json`), so a literal written in a rule is on the record.
- **Timeline events** (`timeline.jsonl`) are not currently filtered. Avoid stamping sensitive fields directly into timeline event payloads.
- **Operational logs** are not affected.
- **The DLQ** preserves the original envelope by design (forensics).


## Reading a continuation

A request that promotes to a continuation has two traces, because the second half can land on another machine: a worker's callback reaches any web node.

- **The origin** is the request up to the suspend. Its last step is the suspending op, status `pending`, spanning the sync window (`continue_after`).
- **The resume** has the id `resume-<run id>-<stage>`. It holds everything after.

Each links to the other: `txco trace <rid>` prints `origin` or `resume`, the admin page links them, and `--json` has them under `continuation`.

A `mode = "continuable"` op is different from a worker's: it runs here, so what it does inside is traced. A `call://` op's rules, every `cap://` capability they call and every `ai://` call they make are steps on the resume trace, which starts when the op started. A continuable op that finishes inside `continue_after` has them on the request's own trace instead. Either way the op itself has one row, with its real duration. A worker's own work is its own business: a resume after a worker's callback holds what came after.

`--trace-detached-bytes` bounds the bodies one op's work keeps. Past it, a step keeps its sizes and timings and shows as truncated. `0` keeps metadata only.

The steps of a deferred join (`join_at_scope`) are not traced.

### Naming a trace

A list of traces shows routes, and a person reading one wants to know whose run it was. A continuable op can say, in the stack's own words:

```txcl
WHEN ._packet.run =~ /./
  WITH mode = "continuable", trace_tags = &object("pony", ._packet.pony, "run", ._packet.run)
  EXEC "call://agent"
```

Tags appear on both traces of a run, in the list and on the trace, as strings: at most 8 pairs, each key and value cut to 64 characters.

### Model calls

A step that called a model carries what the call was: the kind (`chat`, `decide`, `embed`), provider, model, tokens, latency and retries, or its error code. `txco trace <rid>` shows them in the step table, `--step` in full, and the admin page can show only those steps. A call refused before it reached a provider still shows as one. The prompt and the answer are the step's bodies.

## Long traces

A trace is one document, and a run's can hold thousands of bodies. A backend that buffers a trace to its end can use `trace.BodyBuffer`: past a threshold it moves bodies to a temp file, which is uploaded as it stands when the trace ends, and the document carries a reference per step instead. A body is then fetched alone, `GET /traces/requests/{rid}/body?ref=`, when someone opens that step.

The format is a run of concatenated gzip members, one per step, each `{"in":…,"out":…}`. Each member decodes alone, and an extent is a position in exactly the stored bytes, so a store must keep them as given, with no `Content-Encoding` on the object. A backend implements `trace.BodyStore` and runs `tracetest.BodyStoreConformance`, which fetches every member alone and out of order.

The file backend keeps a pair of bodies over 256 KiB out of the document in the same way, reading it back from the step's folder.

## Open Telemetry Support

If you run your own chassis you can inspect each event via standard [Open Telemetry](https://opentelemetry.io/) signals. 
Just configure your environment and point the chassis there:

```
// Configuration is via standard OTel environment variables:
//   - OTEL_EXPORTER_OTLP_ENDPOINT   (e.g. http://localhost:4318)
//   - OTEL_EXPORTER_OTLP_PROTOCOL   (http/protobuf | grpc)
//   - OTEL_SERVICE_NAME             (default: txco-chassis)
//   - OTEL_RESOURCE_ATTRIBUTES      (extra resource attributes)
```