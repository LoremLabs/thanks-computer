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
bytes and halts. The index lives in memory — files are loaded at boot and rebuilt
on `txco apply` (a dbcache reload), never read off disk on the request path.

## Clean URLs and indexes

`txco://static` resolves paths the way a static host does (`try_files`):

| Request | Serves |
|---|---|
| `/` | `index.html` |
| `/about` | `about.html` |
| `/blog` | `blog/index.html` |
| `/app.js` | `app.js` (exact — a path that already has an extension never falls back) |

Content type is resolved from the extension (a pinned table for the common web
types, then the OS database, then content-sniffing for extension-less files).

## Limits

- **1 MiB per file**, **2048 files**, **64 MiB total** across the workspace
  layers — anything over the cap is skipped at load time (and logged), so a
  runaway `FILES/` tree can't exhaust memory. A single very large asset belongs
  on a CDN.

:::warning
A request path with **any segment that starts with `_`** is never served over
HTTP — it's treated as private (e.g. `FILES/_mail/` email templates: readable by
ops, never public). So a file at `FILES/_app/app.js` returns nothing, not a leak.
This bites SvelteKit's default `_app/` asset dir — see [below](#a-sveltekit-or-any-static-site).
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

## A SvelteKit (or any static) site

[`@txco/svelte-adapter-thankscomputer`](https://www.npmjs.com/package/@txco/svelte-adapter-thankscomputer)
turns a SvelteKit build into a stack: it writes your app into `FILES/` and
generates a small SPA-fallback op, all served by `txco://static`.

```js
// svelte.config.js
import adapter from '@txco/svelte-adapter-thankscomputer';

export default {
  kit: {
    // REQUIRED: txco never serves a `_`-prefixed path, so the default appDir
    // "_app" would 404 every hashed asset. Use any non-`_` name. The adapter
    // throws if you leave it as "_app".
    appDir: 'app',
    adapter: adapter({ out: 'OPS/web' }),
  },
};
```

```sh
npm install -D @txco/svelte-adapter-thankscomputer
vite build      # writes OPS/web/FILES/** + a spa-fallback op
txco apply      # ships the stack to your tenant
```

Prerendered routes serve their own HTML through the `try_files` resolution above;
genuinely client-rendered routes fall through to the generated fallback op (a
catch-all at the stack's last scope) so deep links and hard reloads still render
the app shell.
