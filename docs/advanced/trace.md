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

`--trace-dir` is the root (default `./data/trace`). `--trace-mode=full --trace-dir=/var/log/txco/traces` is a typical production setting when you actually want payloads. In `full` mode, bodies are capped per step by `--trace-body-cap-bytes` (default 65536).

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
    in.json        envelope handed to the handler
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

Trace logs persist whatever the rules touched, byte-for-byte. For production deployments that handle user data — auth tokens in headers, raw mail bodies, classifier outputs containing PII — two reserved `WITH` keys on any rule scrub the trace artifacts at write time, without touching runtime data:

```txcl
WHEN @web.req.url.path = "/checkout"
WITH redact = "_txc.web.req.body, _txc.web.req.headers.authorization"
WITH omit   = "_txc.lmtp.msg.attachments"
EXEC "op://CHECKOUT"
```

| Keyword               | Effect on the trace JSON                                                                 |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `redact = "a.b, c.d"` | Replaces each path's value with the string `"[REDACTED]"`. Field stays; value is masked. |
| `omit = "x.y"`        | Deletes the path entirely. Field vanishes from the trace JSON.                           |

- The rule's own WHEN / SELECT / EXEC still see the full envelope. Only what hits durable storage is affected.
- Paths are exact gjson dot-paths.
- A path listed in both `redact` and `omit` is resolved by **omit wins** (the more aggressive choice).
- Hints are **scoped per `(tenant, stack)`**: a `redact` declared in `acme/support` does NOT bleed into `acme/billing` even though both are tenant `acme`. If a request enters `acme/support` and an EXEC jumps to `acme/billing`, both stacks' hints apply to the trace from that point on (union semantic).
- Hints are **static** — only literal string values are honored. `WITH redact = @some.path` (runtime resolution) is silently skipped, since the trace sink must know the paths at write time.
- The chassis collects hints at startup and rebuilds the registry on every dbcache reload (so `txco apply` picks up new hint declarations without a restart).
- Application order is **omits first**, then redacts. Applying omits first lets `_txc.lmtp.msg` vanish without wasting a sentinel write on `_txc.lmtp.msg.headers.authorization`.

### Where it runs

The redaction layer lives below the async-sink boundary, so in async mode the work happens on the trace worker goroutine — off the request hot path. Per-write cost is one `gjson.Get` existence check plus an `sjson.Set` or `sjson.Delete` per declared path; sub-millisecond for typical envelopes.

In sync mode (`--trace-async=false`) redaction runs on the request goroutine just before the disk write. Still cheap; the disk fsync that follows is the dominant cost.

### What's NOT redacted

- **Runtime data** is never touched. The rule's matchers, the op's input, the merged response all see real values.
- **Timeline events** (`timeline.jsonl`) are not currently filtered. Avoid stamping sensitive fields directly into timeline event payloads.
- **Operational logs** (zap stdout) are not affected — apply log redaction at the logger if needed.
- **The DLQ** preserves the original envelope by design (forensics).
