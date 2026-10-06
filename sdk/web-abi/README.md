# @txco/web-abi

The conformance kit for the [Thanks, Computer](https://www.thanks.computer) (TxCo) Web ABI: the contract a framework's build
produces so `txco` can deploy it.

```
<out>/
  txco-web.json     the manifest: what the build is, never its routing
  public/           files, installed as the stack's FILES/
  server/           an optional Fetch handler: export default { fetch(request, ctx) }
  ops/              ordinary .txcl, in scope directories (ops/900000/…)
```

The kit has four parts:

- **The manifest schema** (`txco-web.schema.json`) and a validator. `txco`
  embeds the same schema file, and both validators pass the same test corpus.
- **The envelope ↔ Fetch bridge**, which every runner uses. It turns the
  chassis's request envelope into a Fetch `Request`, calls the handler, and
  turns the `Response` into the delta the chassis merges.
- **A harness** that checks a build's `server/` before any runner exists, and
  serves a build locally.
- **The producer helpers** (`@txco/web-abi/producer`) that a framework adapter,
  preset or plugin writes a build with. They render the ops (a navigation op
  and the catch-all), write and validate the manifest, and guard the output
  directory before a build wipes it. Producers built on them answer requests
  the same way.

`txco web check <out>` checks the rest: the files, the ops, and every request
the build has to answer.

## The manifest

```json
{
  "abi": 1,
  "server": { "entry": "server/index.mjs" },
  "immutable": ["_app/immutable/"]
}
```

| Field          | Meaning                                                                               |
| -------------- | ------------------------------------------------------------------------------------- |
| `abi`          | `1`                                                                                   |
| `server.entry` | The handler module, under `server/`. Leave it out for a static build.                 |
| `immutable`    | `public/` prefixes whose file names carry a content hash. They are cached for a year. |

Keys starting with `x-` are free for producers. Any other key is an error.

## The server contract

```js
export default {
  async fetch(request, ctx) {
    // a Fetch Request; ctx = { client: { ip } }
    return new Response("…");
  },
};
```

- **Buffered both ways.** An answer is capped at about 3 MiB of body.
- **Cookies.** Each `Set-Cookie` arrives on its own line.
- **Errors.** A handler that throws, or returns something other than a
  `Response`, answers `500` without revealing why.

## Use

```sh
npx txco-web-abi validate <out>                 # the manifest
npx txco-web-abi check-server <out> [--json]    # drive server/ through the bridge
npx txco-web-abi serve <out> [--port 8787]      # public/ first, then server/
```

```js
import {
  dispatch,
  envelopeToRequest,
  responseToDelta,
} from "@txco/web-abi/bridge";
import { validateManifest } from "@txco/web-abi/manifest";
```

## Write a producer

```js
import { checkOutDir, renderOps, writeManifest, writeOps } from "@txco/web-abi/producer";

// Before the build: refuse an output dir inside OPS/, one holding foreign
// files, or one holding the project.
const problems = checkOutDir({ dir: out, protect: [root, outDir], notInside: [outDir], entries });

// After it: public/ is in place. One navigation op (the shell with 200 for
// an app that routes in the browser, or the 404 page with 404), plus the
// catch-all at 900900.
await writeOps(out, renderOps({ mode: "spa", producer: "my-adapter", page: { name: "index.html", html } }));
await writeManifest(out, { abi: 1, immutable: ["assets/"], "x-producer": { name: "my-adapter" } });
```

`@txco/vite-plugin` is built this way.

## Develop

```sh
npm install
npm test     # tsc, then node --test (Node 20+)
```

## License

MIT
