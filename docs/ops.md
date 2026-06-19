# Operations

An operation is the unit of work in [Thanks, Computer](https://www.thanks.computer).

### The contract is always the same:

1. The event arrives as JSON, 
2. If the event [resonates](./resonators.md) for the current step, the operations **executes**,
3. The operation returns JSON, 
4. The [TxCo "chassis"](./running.md) deep-merges into the shared document the event carries forward to the next step.

```stack
a simple hello world operation stack

100 hello_world
```

Each
operation is gated by a [**resonator**](./resonators.md)
(its TXCL rule), so it only runs when its condition
matches. 

Operation execution takes a few different shapes, allowing you to run with or without existing infrastructure.

| Shape          | `EXEC` target | Runs                            | Reach for it when                                  |
| -------------- | ------------- | ------------------------------- | --------------------------------------------------- |
| Resonator-only | _(none)_      | on chassis, quickest            | routing, defaults, synthetic responses              |
| Nano-op        | `op://NAME`   | on chassis, nano-op, JS sandbox | logic runs in sandbox environment, no external libs needed         |
| HTTP service   | `http(s)://…` | your service, any language      | existing services, heavy or stateful work, scale    |

## Resonator-only — no code at all

The simplest operation does all its work in the chassis resonator:

```txcl
WHEN @web.req.url.path == "/health"
EMIT .status = "ok", @halt = true
```

No dispatch, no deployment — the resonator rule itself shapes the flow. Use it
for defaults, derived fields, routing, and answers that need no backend. Several [built-in functions](./advanced/txcl/txcl.md#functions) are available to handle common tasks like Base64 encoding.

## Nano-op — `EXEC "op://NAME"`

A [nano-op](./authoring/nano-ops.md) is JavaScript or TypeScript that runs *inside the chassis's sandbox* —
compiled to WebAssembly (Wasm), it is sandboxed (no filesystem, network, or ambient
environment), no service to stand up:

```js
import { op } from "@txco/op";

export default op(async ({ input }) => {
  return { tier: input.amount > 1000 ? "vip" : "standard" };
});
```

Drop `classify.js` in the same directory as its rule, reference it with
`EXEC "op://classify"`, and `txco apply` builds and ships it. Adding a
nano-op costs kilobytes — the JS engine ships once with the chassis.

## External HTTP call — `EXEC "https://…"`

For maximum flexibility and backwards compatibility, you can write operations using any 
HTTP handler _in any language_ that supports JSON (JavaScript, Python, Java, Go, Rust, PHP, Perl, ...). 

```js
// Node, no deps — a complete operation. Use your own framework if you prefer.
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
response merges into the flow like any other operation. `WITH timeout = 2000`
bounds the call; `WITH secrets.headers.authorization.secret = "API_KEY"`
splices a stored credential into the request without writing it in the
rule.

## Start small, grow without rewiring

All three shapes speak the same JSON-merge contract. A step can begin
life as a resonator-only stub, become a nano-op when it needs logic,
and later point at a full service — without touching the rules around
it.

## During development: Mocks

There actually is a fourth type of operation, [the mock](./authoring/mocks.md). You can use mock files
to simulate the response of a third party service during development so you can
work on a single operation at a time without having to stand up the entire distributed
stack. 