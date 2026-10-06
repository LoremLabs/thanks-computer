# The Web ABI — deploy a framework's build

_A framework's build becomes a TxCo stack through one contract: files, an
optional Fetch handler, and ops. `txco` installs it; it never compiles it._

A producer — a framework adapter, a Nitro preset, a Vite plugin — writes a
**Web ABI directory**:

```
<out>/
  txco-web.json     the manifest: what the build is, never its routing
  public/           files, installed as the stack's FILES/
  server/           an optional Fetch handler module
  ops/              ordinary .txcl in scope directories, e.g. ops/900000/fallback.txcl
```

**Files are content. Fetch handlers are computation. Ops are behaviour.**
There is no routing language in the manifest: whatever the framework needs —
an SPA fallback, a 404 page, a server dispatch — it writes as ordinary ops.

## Producers

| Framework | Producer | A navigation to a page that doesn't exist gets |
| --- | --- | --- |
| SvelteKit | [`@txco/svelte-adapter-thankscomputer`](../../sdk/svelte-adapter-thankscomputer/) 0.3 | the shell with 404, from SvelteKit's route table; a known client route gets the shell with 200 |
| Nuxt, Analog, SolidStart (Nitro 2) | [`@txco/nitro-preset`](../../sdk/nitro-preset/) (`thanks-computer`) | `404.html` with 404 for a prerendered site; the shell with 200 for an `ssr: false` app |
| React Router 7 and 8 | [`@txco/react-router`](../../sdk/react-router/) | the shell with 404, from React Router's route table; a known route gets the shell with 200 |
| Astro | [`@txco/astro`](../../sdk/astro/) | `404.html` with 404 |
| Plain Vite (React, Vue, Solid, Preact, vanilla) | [`@txco/vite-plugin`](../../sdk/vite-plugin/) | `index.html` with 200 for a single-page app; `404.html` with 404 for a multi-page one |

All of them end in the same catch-all. A new producer writes its ops with
[`@txco/web-abi/producer`](../../sdk/web-abi/), which renders the same rules. A Nitro build also has `server/` when the
framework builds one (`nuxt build`); deploy its static half with
`--static-only` until a runner exists.

## Bind a stack to a build

Name the build in `txco.yaml`:

```yaml
stacks:
  web:
    abi: www/txco-web       # relative to the workspace root
```

From then on `txco apply`, `txco push web`, `txco dev`, `txco status` and
`txco lint` lay the build over the stack's own tree:

- `public/` becomes the stack's `FILES/`;
- `ops/` joins its ops;
- a marker for each `_` path and immutable prefix (below);
- a provenance file listing what the build owns.

The rules for the binding:

- **Outside `OPS/`.** The path must lie outside `OPS/`, where it would be read
  as a nested stack. `..` is fine.
- **No `OPS/` tree needed.** A stack can exist only through its build, as a
  pure framework site does.
- **Mixed apps.** A mixed app keeps its own `OPS/web/` tree: API ops, private
  `FILES/`, channel stacks, `OUTLETS/`. The build is laid over it.
- **Collisions are errors.** That covers:
  - the same op or file on both sides;
  - an author file under a `_` path or immutable prefix the build claims;
  - any author `FILES/_txco/`.
- **Strict parsing.** The `stacks:` block is parsed strictly. A mistake is an
  error, never a silently dropped binding.

The producer wipes and rewrites `<out>` on every build, so a stale generated op
can't survive. Gitignore all of `<out>`: it's generated from source, and a
producer's ops can embed build output (the SvelteKit adapter's fallback ops
carry the page shell, asset hashes included), so they change on every build.
Build before you push: `txco` refuses a binding whose build is missing.

## The manifest

```json
{ "abi": 1 }
{ "abi": 1, "server": { "entry": "server/index.mjs" } }
{ "abi": 1, "immutable": ["_nuxt/", "app/immutable/"] }
```

| Field          | Meaning |
| -------------- | ------- |
| `abi`          | `1` |
| `server.entry` | The Fetch handler module, under `server/`. Leave it out for a static build. |
| `immutable`    | `public/` prefixes whose file names carry a content hash. They are served with `Cache-Control: public, max-age=31536000, immutable`; HTML always revalidates. |

Keys starting with `x-` are free for producers. Any other key is an error.
The schema is `sdk/web-abi/txco-web.schema.json`; `txco` and the
[conformance kit](#the-conformance-kit) both validate against it.

## How a request runs

```
_sys boot (platform-owned)
  scope 0     detect-tenant: hostname → tenant + stack
  scope 50    txco://static: a public/ file answers (GET/HEAD) and halts
  scope 100   txco://route: jump into the stack
                  │
                  ▼
the stack
  the author's ops, below 900000      (APIs, auth, data 404s, …)
  the build's ops, 900000 and up      (fallback, 404 page, server dispatch, catch-all)
```

**The stack is the application's hook.** The platform decides when app code
runs: after tenant detection and static files. The app decides what it does.
Nothing is installed into `_sys`.

- **Producer ops go last.** A fallback that ran before the author's ops would
  swallow GET navigations those ops answer, such as email links or OAuth
  callbacks. The band from 900000 up is the producer's, and `txco lint` warns
  about an author op there.
- **Static answers first, so:**
  - an op can't shadow a file in `public/`;
  - an op can't add headers to a static response.

### Every build ends in a terminal op

A routed stack that finishes without answering doesn't get a 404: the web head
sends 200 with the envelope as JSON, which APIs rely on. So a build must answer
every request that reaches its end, of any method and any path.

That takes a navigation op **and** a catch-all:

```txcl
# ops/900000/spa-fallback.txcl — the shell for a page navigation
WHEN @src == "http" && (@web.req.method == "GET" || @web.req.method == "HEAD")
     && @web.req.url.path !~ /(?i)\.[a-z0-9]+$/
  EMIT @web.res.status = 200,
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.body = b64"<!doctype html>…",
       @halt = true
```

```txcl
# ops/900900/not-found.txcl — everything else
WHEN @src == "http"
  EMIT @web.res.status = 404,
       @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
       @web.res.body = b64"404 not found\n",
       @halt = true
```

The catch-all sits at a later scope because ops in one scope run concurrently.
A multi-page build serves `404.html` with 404 instead of a shell. A framework
with a reliable route table may give unknown routes the shell with 404, as the
SvelteKit adapter does.

A plain SPA fallback answers an unknown client route with 200 and the shell;
the client router renders its not-found view. Where the status matters,
prerender the route, add an op that knows it, or render on the server.

An empty body doesn't answer: a 200 or 302 whose `@web.res.body` is empty
falls through to the envelope JSON. Give a redirect a short body.

## `_` paths

A path with a `_`-prefixed segment is private in a stack: readable by ops,
never served. Files installed from `public/` are the exception. The build put
them there to be public, so the installer marks each one and static serves it.
That's what makes framework defaults work unchanged:
- SvelteKit's `_app/`, Astro's `_astro/`, Nuxt's `_nuxt/`;
- a bundler's `_`-prefixed chunk hash.

The author's own `FILES/_mail/` stays private.

A chassis too old to read the markers would store them as private files and
404 every `_` asset. So `txco apply` checks the chassis's `/healthz` for the
`web-abi-markers` feature, and refuses without it.

## `server/` (not yet run)

`server/` holds one module in the Workers shape:

```js
export default {
  async fetch(request, ctx) {   // ctx = { client: { ip } }
    return new Response("…")
  }
}
```

No chassis runs it yet, so:
- `txco apply` refuses a build with a server entry;
- `--static-only` deploys its static half and leaves the entry out.

The bridge between the chassis envelope and Fetch is fixed: the
[conformance kit](#the-conformance-kit) implements it, and every runner will
use it.

## Check a build

```sh
txco web check www/txco-web      # or: txco web check web
```

**What it does.** It installs the build alone, on a throwaway chassis (free
ports, a temporary directory, its own `TXCO_HOME`), and probes it:

| Probe | Passes when |
|---|---|
| `/` | `public/index.html` is served |
| a plain file | 200, the content-hash ETag, the same bytes |
| a `_` file | it's served |
| an immutable file | `Cache-Control: public, max-age=31536000, immutable` |
| an unknown page | answered with HTML (200 or 404) |
| an unknown `.js` | 404 |
| a POST nothing handles | 4xx |
| HEAD of the page and of the file | same status as the GET, no body |
| `If-None-Match: W/"…"` | 304 |

**Unanswered requests.** A request no op answers fails, naming the missing
terminal op.

**Offline checks first.** A bad manifest, a bad op or a server entry fails
before anything boots.

**Output and exit.** `--json` gives the report as JSON, and `--strict` fails
on warnings too. It exits 1 on failure.

## The conformance kit

`@txco/web-abi` (`sdk/web-abi`) has three parts:
- the manifest validator;
- the envelope ↔ Fetch bridge;
- a harness: `txco-web-abi check-server <out>` drives a build's `server/`
  through the bridge, and `txco-web-abi serve <out>` serves a build locally.

## Moving from adapter 0.2

Adapter 0.2 wrote into the stack's tree. Under the ABI:
1. Point the adapter's `out` at a directory outside `OPS/`.
2. Bind the stack in `txco.yaml`.
3. Delete the old `OPS/<stack>/FILES/` and the generated scope-900000 ops.
