# Operations

An **operation** is the unit of work an event flows into. Each is gated by a
**resonator** (its `WHEN`/`SET`/`SELECT`/... clauses, written in
[TXCL](./txcl.md)); when the resonator fires, the operation runs and whatever it
produces **deep-merges** back into the event. Operations come in three shapes.

| Kind           | `EXEC` target | Runs                           | Use it for                                                       |
| -------------- | ------------- | ------------------------------ | ---------------------------------------------------------------- |
| Resonator-only | _(none)_      | in TXCL, no dispatch           | shaping the flow, routing, synthetic responses                   |
| HTTP service   | `http(s)://…` | your service, over HTTP        | existing services, heavy or stateful work, any language at scale |
| Nano-op        | `op://NAME`   | sandboxed JS/TS on the chassis | small, pure, fast logic with no service to deploy                |

_(Two more `EXEC` schemes exist — `txco://` built-ins and `mcp+https://` tools —
see [txcl.md](./txcl.md#exec--dispatch-target).)_

---

## 1. Resonator-only (no `EXEC`)

The simplest operation does all its work in TXCL — no external call. It shapes
the event with `SET` (before dispatch) and `EMIT` (onto the response), gated by
`WHEN`. With no `EXEC`, nothing is dispatched; the resonator just contributes to
the flow.

```txcl
# Answer a health check directly, no backend.
WHEN @web.req.url.path == "/health"
EMIT .status = "ok",
     @web.res.status = 200,
     @halt = true
```

Reach for this to set defaults or derived fields, synthesize a response, redact
trace data (`WITH redact = …`), or route (`EMIT @goto = "billing/0"`). Values can
be literals or function calls — `&uuid()`, `&now("rfc3339")`, `&json(…)` — see
[Functions](./txcl.md#functions).

---

## 2. HTTP service — `EXEC "http(s)://…"`

`EXEC "https://…"` dispatches the event to any HTTP service. The contract is
plain JSON in, JSON out — if you can write an HTTP handler, you can be an op.

**Request.** By default the chassis sends a `POST` with the **event envelope as
the JSON body** (`Content-Type: application/json`) and `User-Agent: txco/<ver>`.
For third-party GET APIs, set `WITH method = "GET"` (also `PUT`/`PATCH`/`DELETE`/
`HEAD`); body-less methods send no body, so put parameters in the URL.

Two fields are stamped on the outbound envelope so a handler knows which op it's
serving:

| Field       | Value                                      |
| ----------- | ------------------------------------------ |
| `_txc.op`   | the firing op's identity, `<stack>/<name>` |
| `_txc.step` | its scope, as an integer                   |

**Response.** Return JSON; it's **deep-merged** into the event (objects recurse,
arrays append, scalars overwrite). Return `{}` to contribute nothing. A handler
can also steer the chassis by including control fields in its response —
`_txc.halt`, `_txc.goto`, `_txc.web.res.*` — exactly as a built-in would (see
[control flow](./txcl.md#control-flow-via-_txc)).

**Directives.** `WITH timeout = 2000` bounds the call at 2s (capped by
`--op-timeout-max`). To pass a credential without writing it into the resonator,
`WITH secrets.headers.authorization.secret = "API_KEY"` splices the materialized
secret into the request at dispatch — see the
[secret store runbook](./runbook-secret-store.md).

```txcl
WHEN .user.id =~ /^u_/
WITH timeout = 2000
WITH secrets.headers.authorization.secret = "API_KEY", secrets.headers.authorization.format = "Bearer {}"
EXEC "https://api.example.com/enrich"
EMIT .enriched_at = &now("rfc3339")
```

A handler in any language is just: read JSON from the request body, return JSON.

```js
// Node, no deps
http
  .createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const env = JSON.parse(body || "{}");
      res.setHeader("content-type", "application/json");
      res.end(JSON.stringify({ tier: env.amount > 1000 ? "vip" : "standard" }));
    });
  })
  .listen(9000);
```

---

## 3. Nano-op — `EXEC "op://NAME"`

`EXEC "op://NAME"` runs a small operation **inside the chassis** — no service to
deploy, no network hop. You author it in JavaScript or TypeScript with the
[`@txco/op`](../sdk/op) SDK; it compiles to WebAssembly and runs on the chassis's
sandbox (no filesystem, network, or ambient environment).

Author a `<name>.js` next to its resonator:

```js
import { op } from "@txco/op";

export default op(async ({ input, env, secrets, log }) => {
  log.info("classifying", input.id);
  return { tier: input.amount > 1000 ? "vip" : "standard" };
});
```

The handler receives one `ctx`:

| `ctx` field    | What it is                                                           |
| -------------- | -------------------------------------------------------------------- |
| `input`        | the event envelope                                                   |
| `meta`         | trace identity — `{ rid, op, stack, scope, name }`                   |
| `env`          | the resonator's `WITH`-clause config                                 |
| `secrets`      | per-op secrets by name (same store as an HTTP op's `WITH secrets.*`) |
| `log` / `emit` | structured logging / optional events                                 |

Helpers live under subpaths: `@txco/op/{envelope,schema,crypto,codec}`. Whatever
you return deep-merges into the flow, exactly like an HTTP op.

The resonator references it by name:

```txcl
WHEN @web.req.url.path == "/classify"
EXEC "op://classify"
```

`op://classify` resolves to the colocated `classify.js`; `txco apply` (or `txco
dev`) builds it and uploads the content-addressed module. Author and test with
`txco op init / build / run / test`. Full SDK reference: [`sdk/op`](../sdk/op).

Under the hood the build *dynamically links* against a shared QuickJS engine
(Javy): each op compiles to just its own bytecode — roughly a kilobyte — rather
than a self-contained ~1.25 MB module that embeds the whole engine. The engine
ships once with the chassis and is shared by every op, so adding a nano-op costs
kilobytes, not megabytes. Authoring this is automatic, and so is the toolchain:
the build needs a matching `javy`, which TxCo auto-fetches (the pinned release,
checksum-verified) into `~/.config/txco/tools/` on first build and caches there —
no manual install. (Set `TXCO_JAVY=/path/to/javy` to use your own, or
`TXCO_JAVY_NO_DOWNLOAD=1` to forbid the fetch on air-gapped hosts.) The shared
engine provides the event loop, text-encoding, and stream IO — so `async`/`await`,
`TextEncoder`, and the stdin→stdout envelope all work as written.

---

## Choosing a kind

- **Resonator-only** when there's no code to run — defaults, routing, synthetic
  responses, scrubbing.
- **Nano-op (`op://`)** for small, pure, fast logic you'd rather not stand up a
  service for — classification, parsing, hashing, reshaping. Sandboxed.
- **HTTP (`http(s)://`)** for existing services, stateful or heavy work, real
  I/O, or anything in another language or runtime at scale.

All three speak the same JSON-merge contract, so you can start a step as a
resonator-only stub, grow it into a nano-op, and later point it at a full service
— without touching the resonators around it.
