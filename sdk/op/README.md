# @txco/op

Author sandboxed **txco nano-ops** — small functions a resonator invokes with
`EXEC "op://<name>"`, run in-process on a WASI sandbox (no filesystem, network,
or ambient env).

```js
import { op } from "@txco/op";

export default op(async ({ input, env, log, emit, meta }) => {
  log.info("classifying", meta.op);
  emit("classified", { tier: "vip" });
  return { classification: "vip", summary: input.email?.draft?.summary };
});
```

A compute is a `<name>.js` (or `.ts`) sitting next to its resonator at
`OPS/<stack>/<scope>/<name>.txcl`. Build, run, and test it with the CLI:

```sh
txco op init  OPS/site/100/classify      # scaffold classify.js (+ classify.txcl)
txco op build OPS/site/100/classify.js   # → classify.wasm + compute://sha256/…
txco op run   classify.wasm --input '{"name":"Matt"}'
txco op test  OPS/site/100/classify.txcl # runs against the scope's mock-request.json
```

`txco apply` / `txco dev` discover the colocated source automatically, build it,
and upload the content-addressed module — no manifest, no `node_modules`.

## The handler context

`op(handler)` receives one `ctx`:

| field      | what it is                                                            |
| ---------- | --------------------------------------------------------------------- |
| `input`    | the selected request envelope (the op's input)                        |
| `meta`     | trace identity: `{ rid, op, stack, scope, name }`                     |
| `env`      | the op's non-secret config — the resonator's `WITH`-clause channel    |
| `secrets`  | per-op materialized secrets by name (`ctx.secrets.STRIPE_API_KEY`)    |
| `log`      | structured logging (`log.info/warn/error/debug`) → chassis log        |
| `emit`     | `emit(event, data?)` — an optional named event                        |

The handler returns the output envelope (sync or async). `stdout` is just that
envelope; `log`/`emit` go to the chassis log, so they never corrupt the result.

`ctx.secrets` carries the same per-op secrets the chassis would splice into an
`http://` worker's request — declared in the resonator's `WITH secrets:` block,
materialized from the secret store, and handed to the compute by name. They are
cleartext: returning a secret in the output envelope **will** log/trace it.

> `fetch` and `kv` are **not** in this version — they arrive with the host
> capability model.

## Helper subpaths

```js
import { get, set, pick } from "@txco/op/envelope"; // nested path read/write
import { z } from "@txco/op/schema";                // tiny runtime validator
import { b64, sha256, hmac } from "@txco/op/crypto"; // pure-JS crypto
import { json, text } from "@txco/op/codec";         // encode/decode
```

Only what you import is bundled — unused helpers are tree-shaken away.

## TypeScript

`.ts` is first-class (esbuild transpiles it during the build). `op<Input>(…)`
takes an input type parameter for a typed `ctx.input`.

## How it builds

`esbuild` bundles your entry (resolving `@txco/op` from this package, transpiling
TS, tree-shaking) → `javy` compiles the bundle to a WASI module with its event
loop enabled (so `async`/`await` work). The result is content-addressed and runs
on the same wazero engine locally (`txco op run/test`) and in the chassis — so
"works locally" means "works in production".
