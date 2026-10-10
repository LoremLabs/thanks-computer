# Static files — serve a site from a stack

_The web head can serve files straight from your stack — CSS, images, a whole
prerendered site — with no backend, via the built-in `txco://static` op._

Drop a `FILES/` directory in your workspace and `txco apply`. Any request whose
path maps to a file in it is answered directly by the chassis — with the right
content type, a content-hash `ETag`, and `304 Not Modified` on a conditional
GET. No rule to write, no service to run.

## Drop files in `FILES/`

Put assets under `FILES/`, either workspace-wide or inside a stack:

```
my-app/
  FILES/                 # workspace-wide assets
    index.html
    styles.css
    img/logo.svg
  OPS/
    web/
      FILES/             # per-stack assets (take precedence when routed here)
        robots.txt
      100/...
```

`txco apply` uploads the tree (content-addressed — unchanged files dedup, so the
cost is metadata, not bytes). Lookups are **layered, first match wins**:

1. the routed stack's own `OPS/<stack>/FILES/<path>`
2. the workspace-wide `FILES/<path>`
3. an embedded default (`favicon.ico`) as a last resort

## It works out of the box

The bundled `_sys/boot` stack already runs `txco://static` at scope 50 — *before*
routing — so files serve with nothing to configure, even on a host that isn't
routed to a stack yet:

```txcl
WHEN @src == "http"
EXEC "txco://static"
```

The op self-gates: a request that doesn't map to a file returns nothing and the
flow continues to your own rules (or the `404` at the end). A hit emits the file
bytes and halts, so a rule can never shadow a file (or add headers to its
response).

Only `GET` and `HEAD` are static's: any other method goes straight to your rules,
so a `POST /login` reaches your app even when `login.html` exists.

The index is rebuilt on `txco apply` (a dbcache reload), never on the request
path:

- **Workspace files** (`FILES/`, `OPS/<stack>/FILES/` on the chassis's own disk)
  are held in memory, bytes and all.
- **Files a tenant ships with `txco apply`** keep only their path → content-hash
  map in memory. Their bytes come from the content-addressed file store through
  a 64 MiB cache — local disk, or object storage on a fleet.

## Clean URLs and indexes

`txco://static` resolves paths the way a static host does (`try_files`):

| Request | Serves |
|---|---|
| `/` | `index.html` |
| `/about` | `about.html` |
| `/blog` | `blog/index.html` |
| `/app.js` | `app.js` (exact — a path that already has an extension never falls back) |

Only `/` falls back to the root `index.html`; any other unmatched path is a miss.
A path with a segment starting with `.` (`/.env`, `/.well-known/…`) is never a
static file, so your rules can answer it.

A miss under a directory that exists in `FILES/` is a hard `404` (and halts)
when the path looks like a file — nested, with an extension in its last segment.
So `/assets/gone.js` is a `404` once any `assets/*` file exists, instead of
reaching your app; `/assets/page` (no extension) still falls through, for an app
route that shares the prefix.

Content type is resolved from the extension (a pinned table for the common web
types, then the OS database, then content-sniffing for in-memory files without
one).

## Caching

Every file carries a strong `ETag` (its content hash). A conditional `GET` whose
`If-None-Match` names it — in a list, as a weak `W/` tag, or `*` — gets
`304 Not Modified` without the bytes being fetched.

| File | `Cache-Control` |
|---|---|
| HTML, markdown | `max-age=0, must-revalidate` — the entry point always revalidates, so a deploy's new asset URLs are picked up |
| under a prefix marked immutable | `public, max-age=31536000, immutable` |
| anything else | `public, max-age=3600` |

A prefix is marked immutable by a marker file, `FILES/_txco/immutable/<prefix>/_txco_mark`
(its contents are ignored). Mark only directories whose file names carry a
content hash, such as a bundler's `app/immutable/`: those names change whenever
the bytes do. A [Web ABI](./web-abi.md) build's installer writes the markers for you.

Static does not compress (the hosted edge does) and does not answer `Range`
requests.

## Limits

- **Workspace layers:** **1 MiB per file**, **2048 files**, **64 MiB total** —
  anything over the cap is skipped at load time (and logged), so a runaway
  `FILES/` tree can't exhaust memory.
- **Tenant files** (shipped with `txco apply`) have no serving limit. One over
  `--filecas-max-file-bytes` (10 MiB) is served without being cached. A single
  very large asset still belongs on a CDN.

:::warning
A request path with **any segment that starts with `_`** is never served over
HTTP — it's treated as private (e.g. `FILES/_mail/` email templates: readable by
ops, never public). So a file at `FILES/_app/app.js` returns nothing, not a leak.

The one exception is a path its stack marks public, with a marker file at
`FILES/_txco/public/<root>/_txco_mark`, where `<root>` is the path up to and
including its first `_` segment (`_app`, `assets/_chunk.js`). A [Web ABI](./web-abi.md) build's
installer writes these for the files in its `public/` tree — which is how
SvelteKit's `_app/` is served — and anything else under `_` stays private.
:::

## Markdown for agents

A site can answer an agent that asks for markdown with markdown, and a browser
with its HTML, at the same URL:

```
GET /about
Accept: text/markdown

HTTP/1.1 200 OK
Content-Type: text/markdown; charset=utf-8
Vary: Accept
```

Add a `_markdown` inlet beside the stack the hostname is bound to, with the
pages as `.md` files and at least one op:

```
OPS/
  web/
    100/...
    _markdown/
      FILES/index.md         /
      FILES/about.md         /about   (or FILES/about/index.md)
      9000/not_found.txcl    any other page, in markdown
```

Its existence is the opt-in, as for `<stack>/_mail` and `<stack>/_tcp`: there
is nothing to bind or configure, and an inlet gets no hostname of its own.

**Who enters it.** A `GET` or `HEAD` of a page (the root, or a path whose last
segment has no extension) whose `Accept` names `text/markdown` with a q at
least that of `text/html`. `text/html`'s q comes from its most specific match:
`text/html`, then `text/*`, then `*/*`. So:

| `Accept` | Gets |
|---|---|
| `text/markdown` | markdown |
| `text/markdown, text/html;q=0.9` | markdown |
| `text/markdown, text/html` | markdown (naming markdown at all is the signal; no browser does) |
| a browser's, `*/*`, or none | HTML |
| `text/html, text/markdown;q=0.5` | HTML |

Everything else stays on the stack: other methods, and asset paths (`/app.js`,
`/robots.txt`), so an agent still gets those as they are.

**What answers.** Static serves the inlet's `FILES/` first, by the same rules
with `.md` in place of `.html` (`/` → `index.md`, `/about` → `about.md` or
`about/index.md`), as `text/markdown; charset=utf-8` with an ETag. The
inlet's files only: never the stack's, the chassis-wide layer or the embedded
defaults. A page with no file runs the inlet's ops, which answer the rest (a
page built from data, or a markdown 404) and set the content type themselves.

**The headers.** Every page answer on the host, from either stack, static or
op, gets `Vary: Accept`, added to any `Vary` the stack set. An HTML page whose
markdown is a file in the inlet also gets

```
Link: </about.md>; rel="alternate"; type="text/markdown"
```

and that URL serves the file to anyone, whatever their `Accept`, when the
stack has no `about.md` of its own. A page an op renders as markdown isn't
advertised: the chassis can't know it exists without running it. A host
without a `_markdown` inlet gets neither header.

:::note
A stack with no ops counts as withdrawn (`txco deactivate` leaves an empty
version), so an inlet of files alone is never entered: keep at least one op,
such as the markdown 404. A streamed answer doesn't get the two headers.
:::

## Dynamic pages without a backend

Need a page built from *computed* data rather than a file on disk? `txco://web-render`
reads a value from the envelope, optionally renders it, and writes the HTTP
response — see the [builtins reference](./builtins.md):

```txcl
# scope 200 returns what scope 100 produced, rendered as HTML
WITH source = ".text", wrap = "markdown-to-html"
EXEC "txco://web-render"
```

`WITH` options: `source` (envelope path, default `.text`), `wrap` (`raw` | `html`
| `markdown-to-html`), `content_type`, `status`. It always halts. (You can also
shape `@web.res.*` directly in a rule — see the [web inlet](./protocols/web.md).)

## A SvelteKit (or any framework's) site

A framework's build reaches a stack through the [Web ABI](./web-abi.md): the
build's `public/` becomes the stack's `FILES/`, and its ops (an SPA fallback, a
404 page, a catch-all) run after yours. For SvelteKit,
[`@txco/svelte-adapter-thankscomputer`](https://www.npmjs.com/package/@txco/svelte-adapter-thankscomputer)
0.3 writes the build:

```js
// svelte.config.js
import adapter from '@txco/svelte-adapter-thankscomputer';

export default {
  kit: { adapter: adapter({ out: 'txco-web', fallback: '200.html' }) },
};
```

```yaml
# txco.yaml at the workspace root
stacks:
  web:
    abi: www/txco-web
```

```sh
vite build                      # writes www/txco-web/
txco web check www/txco-web     # probes it on a scratch chassis
txco apply                      # ships the stack to your tenant
```

Prerendered routes serve their own HTML through the `try_files` resolution
above. A navigation to a client-rendered route gets the app shell from the
build's `spa-fallback` op, so deep links and hard reloads still render; one
that matches no route gets the shell with a `404` (`spa-404`); anything else
gets a plain `404` (`not-found`).

For Nuxt (and other Nitro frameworks),
[`@txco/nitro-preset`](https://www.npmjs.com/package/@txco/nitro-preset) writes
the same kind of build:

```ts
// nuxt.config.ts
export default defineNuxtConfig({ nitro: { preset: '@txco/nitro-preset' } });
```

```sh
nuxt generate                   # writes txco-web/
txco web check txco-web
txco apply
```

`nuxt generate` prerenders every page it can crawl. A navigation to a page
that doesn't exist gets Nuxt's `404.html` with a `404`, and an `ssr: false`
app gets its shell with a `200` instead.

A plain Vite app (React, Vue, Solid, Preact or vanilla) adds
[`@txco/vite-plugin`](https://www.npmjs.com/package/@txco/vite-plugin). It
copies `dist/` into the build after `vite build`:

```ts
// vite.config.ts
import txco from '@txco/vite-plugin';

export default defineConfig({ plugins: [react(), txco()] });
```

Every page path gets `index.html` with a `200`, so the client router renders
it. A multi-page build gets its `404.html` with a `404` instead.

React Router (framework mode) adds
[`@txco/react-router`](https://www.npmjs.com/package/@txco/react-router) as a
preset, and Astro adds [`@txco/astro`](https://www.npmjs.com/package/@txco/astro)
as an integration:

```ts
// react-router.config.ts
export default { ssr: false, presets: [txco()] } satisfies Config;
```

```js
// astro.config.mjs
export default defineConfig({ integrations: [txco()] });
```

React Router's route table decides the status: a known route gets the shell
with a `200`, any other path the shell with a `404`. An Astro site gets its
`404.html` with a `404`. SolidStart builds through the Nitro preset.
