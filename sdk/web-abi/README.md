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

The kit has three parts:

- **The manifest schema** (`txco-web.schema.json`) and a validator. `txco`
  embeds the same schema file, and both validators pass the same test corpus.
- **The envelope ↔ Fetch bridge**, which every runner uses. It turns the
  chassis's request envelope into a Fetch `Request`, calls the handler, and
  turns the `Response` into the delta the chassis merges.
- **A harness** that checks a build's `server/` before any runner exists, and
  serves a build locally.

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

## Develop

```sh
npm install
npm test     # tsc, then node --test (Node 20+)
```
