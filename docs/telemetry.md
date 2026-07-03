# Telemetry — your application's metrics

_[Traces](./visibility.md) answer "what did this request do?". **Telemetry**
answers "how often does the thing my application cares about happen?" —
signups completed, books queued, checkouts started. A stack emits metric
intents into the envelope; after the request completes, the chassis
validates them and ships them to the metrics backend **you** configure.
Delivery is best-effort and happens off the request path — a down
backend never slows or fails a request._

## Emitting a metric

Write intents to `_txc.telemetry.metrics`, composed with `&array` /
`&object`:

```txcl
WHEN @web.req.url.path == "/subscribe"
  EMIT @telemetry.metrics = &array(
    &object("name",  "book.queued",
            "kind",  "counter",
            "value", 1,
            "attrs", &object("source", "search")))
```

A [nano-op](./authoring/nano-ops.md) can contribute the same way — return
`{"_txc":{"telemetry":{"metrics":[…]}}}`. Because arrays accumulate in the
merge, several operations can each add metrics to one request.

Each metric:

| field   | required | notes                                        |
| ------- | -------- | -------------------------------------------- |
| `name`  | yes      | `domain.action`, e.g. `book.queued`          |
| `kind`  | yes      | `counter` or `histogram` (`gauge` not yet)   |
| `value` | yes      | a JSON number; counters must be ≥ 0          |
| `unit`  | no       | `"1"`, `"ms"`, `"bytes"`, …                  |
| `attrs` | no       | scalar key/values, e.g. `{"plan":"premium"}` |

Keep names low-cardinality and put the varying detail in `attrs` —
`book.queued` with `source: "search"`, not `book.dune.queued`.

## Turning it on

Two tenant secrets are the whole configuration:

```sh
txco secrets set TELEMETRY_ENDPOINT    # your OTLP/HTTP endpoint, e.g. https://ingest.example.com:4318
txco secrets set TELEMETRY_HEADERS     # optional auth headers: "x-api-key=…" (k1=v1,k2=v2)
```

Setting `TELEMETRY_ENDPOINT` turns export on; deleting it turns it off
(within a few minutes — rotation likewise). Without it, intents are
simply dropped. Any OTLP-compatible backend works: SigNoz, Grafana
Cloud, Honeycomb, Datadog, a self-hosted collector. Endpoints must be
`https` (`http` only toward localhost).

## What arrives

Metrics are batched per tenant and exported with context attached:
`service.name` is the tenant slug, plus `txco.tenant`, `txco.node`,
`txco.environment` on the stream and `txco.stack`, `txco.src` on each
point. Nodes export independently — aggregate in the backend
(`sum(book.queued) by txco.tenant`, or `by txco.node` to inspect one
machine).

## Guardrails

Validation drops a bad intent, never the request: unknown kinds,
non-numeric values, and malformed names are discarded and counted in the
chassis diagnostic `chassis.telemetry.dropped`, tagged with a reason.
Attributes are capped (16 per metric, 64 metrics per request, bounded
key/value lengths) and keys that look sensitive — `email`, `token`,
`password`, and the like — are redacted before export.

## The chassis's own signals

There are two telemetry layers, configured independently:

- **Your application's metrics** — this page. Emitted from stacks,
  per-tenant, configured with tenant secrets.
- **The chassis's own signals** — runtime health for whoever operates
  the node: request counts and timings, per-operation durations, and
  diagnostics like `chassis.telemetry.dropped`. Configured per node
  with the standard [OpenTelemetry](https://opentelemetry.io/)
  environment variables:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com:4318
OTEL_SERVICE_NAME=txco-chassis            # the default
OTEL_RESOURCE_ATTRIBUTES=…                # extra resource attributes
```

The layers never mix: the node's `OTEL_*` credentials are not sent to
tenant endpoints, and tenant metrics don't ride the chassis stream.

## Node knobs

`--telemetry-enabled` (default `true`) gates the subsystem;
`--telemetry-exporter` picks the backend: `otlp` (default) or `log`,
which writes each metric as a chassis log line — handy in development,
no secrets needed.
