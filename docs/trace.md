# Trace — see what a flow actually did

_Every event in Thanks, Computer flows through steps of parallel
operations — this page covers how to see exactly what happened,
after the fact. ([Overview](./overview.md))_

Every flow can leave a complete, browsable record: the envelope that
arrived, every operation that fired, what each received and returned,
how long it took, and the final response. No rerun, no debug logging,
no print statements — the answer to "what did this request do?" is
sitting on disk. Each flow is one beat of an [arc](./arcs.md); read
its traces together and you have the arc's story so far.

<img width="1155" height="737" alt="image" src="https://github.com/user-attachments/assets/e6bb68e0-a2f6-4e2d-ba3c-b10a27a8fbc8" />

```sh
txco trace          # recent flows: rid, source, route, duration
txco trace last     # step-by-step table for the most recent flow
txco trace <rid>    # …or any specific one
```

Two operations that ran at the same step show up side by side — the
trace makes parallelism visible. The same explorer lives in the
chassis's web UI, and `txco demo` opens with tracing on, so the
feedback loop while learning is: fire an event, read its trace.

<img width="394" height="209" alt="image" src="https://github.com/user-attachments/assets/85e908e1-7ea6-4f17-ae6d-89e9a7de21b6" />


## Dial in how much is kept

`--trace-mode` controls the cost:

| Mode      | What's written                                              |
| --------- | ----------------------------------------------------------- |
| `off`     | Nothing (the default). Zero cost.                           |
| `summary` | Timings, sizes, what fired — no payload bytes.              |
| `full`    | Everything, including each operation's input and output.    |

Traces are plain JSON files under `--trace-dir`, one folder per
request — greppable, shippable, diffable with the tools you already
have.

## Keeping secrets out

Traces persist whatever the rules touched, so any rule can scrub its
own trail — without affecting the live data the flow computes on:

```txcl
WITH redact = "_txc.web.req.headers.authorization"   # value → "[REDACTED]"
WITH omit   = "_txc.lmtp.msg.attachments"            # field vanishes
```

## Debuggable for AI, too

A trace is structured JSON describing exactly what ran and why — which
makes it as legible to an AI assistant as to you. "Read the trace and
tell me why the billing op didn't fire" is a question an agent can
actually answer.
