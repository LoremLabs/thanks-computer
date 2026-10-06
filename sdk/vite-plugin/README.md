# @txco/vite-plugin

A [Vite](https://vite.dev) plugin for [Thanks, Computer](https://www.thanks.computer)
(txco). It's for plain Vite apps: React, Vue, Solid, Preact or vanilla, with no
meta-framework. After `vite build`, it writes a **Web ABI build** beside your
`dist/`:

- a copy of `dist/` as `public/`;
- the ops a browser-routed app needs, as `ops/`;
- a `txco-web.json` manifest.

`txco apply` installs the build into a stack.

```sh
npm install -D @txco/vite-plugin
```

Vite 6, 7 or 8.

## Usage

```ts
// vite.config.ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import txco from "@txco/vite-plugin";

export default defineConfig({
  plugins: [react(), txco()],
});
```

Bind the build to a stack in your workspace's `txco.yaml`. The path is
relative to the workspace root, and must lie outside `OPS/`:

```yaml
stacks:
  web:
    abi: txco-web
```

Then build, check and deploy:

```sh
vite build                     # writes dist/ as usual, and txco-web/
txco web check txco-web        # installs it on a scratch chassis and probes it
txco apply                     # or: txco push web
```

Gitignore `txco-web/`. It's all build output, and the fallback op embeds
`index.html`, asset hashes included, so it changes on every build.

## What it writes

```
txco-web/
  txco-web.json                  { "abi": 1, "immutable": ["assets/"], … }
  public/                        a copy of dist/
  ops/900000/spa-fallback.txcl   200 + index.html, for any page navigation
  ops/900900/not-found.txcl      a plain 404 for everything else (an asset miss, a POST)
```

`dist/` is left as Vite wrote it, so `vite preview` and anything else that
reads it still works.

**A single-page app** (one `index.html`) routes in the browser. Every page
path that no file or op of yours answers gets `index.html` with status 200.
The client router renders its own not-found view for a path it doesn't know.
There's no route table to check, so this is the Web ABI's plain fallback.

**A multi-page app** (several HTML inputs in `build.rollupOptions.input`)
writes `ops/900000/page-404.txcl` instead. A path with no page gets
`404.html` with status 404 if the build has one, from `public/404.html` or an
input page, and a plain 404 page if not.

**`base`.** With `base: "/app/"` the site lands under `public/app/`, and is
served at `/app/`. A full-URL `base` (a CDN) loads its assets from there; the
plugin warns.

**Caching.** Vite's hashed output under `assets/` is cached for a year. A
file you put in `public/assets/` yourself isn't hashed, and the plugin warns
about it.

**Dot paths** (`public/.well-known/…`) never deploy; the plugin warns and
leaves them out. Library and SSR builds write nothing.

## Options

| Option      | Default        | Meaning |
| ----------- | -------------- | ------- |
| `out`       | `"txco-web"`   | The Web ABI directory to write, relative to the project root. It must be a directory of its own, outside `OPS/` and `dist/`. |
| `spa`       | detected       | `true` answers every unknown page path with `index.html` and 200; `false` with the 404 page. |
| `immutable` | `["assets/"]`  | The `public/` prefixes cached for a year. |
| `scope`     | `900000`       | The navigation op's scope. The catch-all goes 900 above it. |

## Safety

The plugin wipes and rewrites `out` on every build. Before the build starts,
it refuses an `out` that:

- is inside `OPS/`;
- is inside `dist/`;
- holds anything that isn't part of a Web ABI build;
- holds your project.

## License

MIT
