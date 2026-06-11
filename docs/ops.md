# Operations

_In Thanks, Computer, events flow through steps and **operations** are
what runs at each step — this page covers the three shapes an operation
can take. ([Overview](./overview.md))_

An operation is the unit of work. The contract is always the same — the
event arrives as JSON, the operation returns JSON, and what it returns
deep-merges into the shared document, advancing the [arc](./arcs.md)
the event belongs to. Each operation is gated by a **resonator**
(its [TXCL](./txcl.md) rule), so it only runs when its condition
matches. The three shapes differ only in *where the work happens*:

| Shape          | `EXEC` target | Runs                            | Reach for it when                                  |
| -------------- | ------------- | ------------------------------- | --------------------------------------------------- |
| Resonator-only | _(none)_      | in the rule itself, no dispatch | routing, defaults, synthetic responses              |
| Nano-op        | `op://NAME`   | sandboxed JS/TS on the chassis  | small, pure logic with no service to deploy         |
| HTTP service   | `http(s)://…` | your service, any language      | existing services, heavy or stateful work, scale    |

## Resonator-only — no code at all

The simplest operation does all its work in the rule:

```txcl
WHEN @web.req.url.path == "/health"
EMIT .status = "ok", @halt = true
```

No dispatch, no deployment — the rule itself shapes the flow. Use it
for defaults, derived fields, routing, and answers that need no backend.

## Nano-op — `EXEC "op://NAME"`

A nano-op is JavaScript or TypeScript that runs *inside the chassis* —
compiled to WebAssembly, sandboxed (no filesystem, network, or ambient
environment), no service to stand up:

```js
import { op } from "@txco/op";

export default op(async ({ input }) => {
  return { tier: input.amount > 1000 ? "vip" : "standard" };
});
```

Drop `classify.js` next to its rule, reference it with
`EXEC "op://classify"`, and `txco apply` builds and ships it. Adding a
nano-op costs kilobytes — the JS engine ships once with the chassis.

## HTTP service — `EXEC "https://…"`

Any HTTP handler in any language can be an operation: read the event
envelope from the request body, return JSON.

```js
// Node, no deps — a complete operation
http.createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const env = JSON.parse(body || "{}");
    res.end(JSON.stringify({ tier: env.amount > 1000 ? "vip" : "standard" }));
  });
}).listen(9000);
```

Point a rule at it — `EXEC "https://api.example.com/enrich"` — and its
response merges into the flow like any other op. `WITH timeout = 2000`
bounds the call; `WITH secrets.headers.authorization.secret = "API_KEY"`
splices a stored credential into the request without writing it in the
rule.

## Start small, grow without rewiring

All three shapes speak the same JSON-merge contract. A step can begin
life as a resonator-only stub, become a nano-op when it needs logic,
and later point at a full service — without touching the rules around
it.

Beyond these three: `EXEC "ai://chat"` puts [an AI model in the
flow](./ai.md), `EXEC "mcp+https://…"` calls
[an agent tool](./advanced/protocols/mcp.md), and `txco://` names a
[chassis builtin](./advanced/builtins.md) — static files, outbound
email, HMAC, and more.
