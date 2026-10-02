# Nano-ops — Run operations at the edge

_Small, pure logic via JavaScript that runs sandboxed on the chassis — no service to
deploy._

```sh
txco op init OPS/support/0100_TRIAGE/classify    # scaffold classify.js next to its rule
txco op run  OPS/support/0100_TRIAGE/classify.js --input '{"amount": 1200}'
txco op test OPS/support/0100_TRIAGE/classify.js # mock-request.json in, diff vs mock-response.json
txco apply                                        # build + upload happen automatically
```

The handler is a function of the event:

```js
import { op } from "@txco/op"; // helper library imports txco functions

export default op(async ({ input, env, secrets, log }) => {
  return { tier: input.amount > 1000 ? "vip" : "standard" };
});
```

and the rule references it by name — `EXEC "op://classify"` resolves
to the colocated `classify.js` at apply time.

What the tooling handles for you:

- **Building.** `apply` (and `txco dev` on save) bundles, compiles to
  wasm via the pinned `javy` (auto-fetched and checksum-verified on
  first use), and uploads the content-addressed module.
- **Running locally.** `txco op run` executes on the same engine
  production uses — what passes locally is what ships.
- **Testing.** `txco op test` uses the scope's
  [mock fixtures](./mocks.md): `mock-request.json` as input, diff
  against `mock-response.json` (exit 1 on mismatch), with
  `mock-env.json` / `mock-secrets.json` standing in for `ctx.env` and
  `ctx.secrets`.
- **Sandboxing.** No filesystem, network, or ambient environment;
  memory and wall-clock capped per call
  ([runtime reference](../advanced/serve.md)).
- **Showing the source.** `apply` also keeps each nano-op's source: the
  entry file and the local files it imports, not the SDK. In the admin,
  click a `compute://sha256/…` in an op or a trace to read it. The source
  is stored once by hash, so an unchanged nano-op adds only a small row to
  each new stack version. A prebuilt `.wasm` from a package has no source
  to show. The first `apply` after upgrading the chassis makes one new
  version of each stack that has nano-ops, to record their source.
  `txco apply --no-source` (or `push`) leaves source out; the new version
  then carries none.

## Important restrictions when choosing nano-ops

Nano-ops are not meant for running large operations requiring IO, but instead as a way to have a richer language
available for processing and manipulating operation state in the lowest runtime latency possible. This means
that you won't compile a node app with a full `node_modules` included. It also means that many functions that node
includes, like `fetch` won't be available to the nano-op.


## See also
Full txco handler API: (`ctx` fields, helper subpaths
`@txco/op/{envelope,schema,crypto,codec}`): the
[`sdk/op` README](../../sdk/op/README.md).
