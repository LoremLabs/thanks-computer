# @txco/svelte-adapter-thankscomputer

A [SvelteKit](https://svelte.dev/docs/kit) adapter that deploys your app to
**thanks.computer** (txco). It writes a fully static build into a stack's
`FILES/` directory — served by the built-in `txco://static` op with strong
ETags and conditional GET — and generates a small **SPA-fallback op** so client
routes resolve on a hard reload. Deploy the result with `txco apply`.

```sh
npm install -D @txco/svelte-adapter-thankscomputer
```

## Usage

```js
// svelte.config.js
import adapter from "@txco/svelte-adapter-thankscomputer";

export default {
  kit: {
    // REQUIRED: txco never serves a path whose segment starts with "_", so
    // SvelteKit's default appDir ("_app") would 404 every hashed asset.
    appDir: "app",
    adapter: adapter({
      out: "OPS/web", // the stack dir holding FILES/ and scope folders
    }),
  },
};
```

Then build and deploy:

```sh
vite build       # writes OPS/web/FILES/** and OPS/web/900000/spa-fallback.txcl
txco apply       # ships the stack to your tenant
```

Your app is now served at the stack's hostname — assets straight from
`txco://static`, every client route falling back to the app shell.

## Why `appDir` matters

txco treats any request path with a `_`-prefixed segment as private (readable by
ops, never served over HTTP — e.g. `FILES/_mail/` templates). SvelteKit's default
`appDir` is `_app`, so `/_app/immutable/*.js` — i.e. all your hashed JS/CSS —
would silently fail to load. Setting `appDir` to a non-underscore name (`app`,
`assets`, …) fixes it. The adapter **throws** if you leave it as `_app`.

## How it works

- `vite build` runs the adapter, which writes client assets and any prerendered
  pages into `<out>/FILES/`, plus a SPA fallback page (default `index.html`).
- `txco://static` (the boot stack, scope 50) serves any real file under `FILES/`
  for the routed tenant — with content-type, a content-hash ETag, and 304s.
- `txco://static` also does `try_files` resolution: `/` → `index.html`, `/about` →
  `about.html`, `/blog` → `blog/index.html`. So **prerendered** routes and the
  homepage serve their own HTML directly.
- A genuinely **client-rendered** route (no prerendered file) has nothing to
  resolve, so static falls through and the generated `spa-fallback.txcl` op serves
  the app shell — deep links and reloads still render correctly.

## Options

| Option          | Default        | Description                                                                                  |
| --------------- | -------------- | -------------------------------------------------------------------------------------------- |
| `out`           | `"OPS/web"`    | Stack directory holding `FILES/` and scope folders; build lands in `<out>/FILES/`.           |
| `fallback`      | `"index.html"` | SPA fallback page filename, or `false` to disable SPA mode (serve only prerendered files).   |
| `fallbackOp`    | `true`         | Generate `<out>/<fallbackScope>/spa-fallback.txcl` to serve the shell for client routes.     |
| `fallbackScope` | `900000`       | txcl scope for the generated fallback op. Very high by default so this catch-all runs last (after `txco://route` and any ops you add) and leaves the whole 1…899999 range free for your own ops. |
| `apply`         | `false`        | Run `txco apply` in the current directory after building, to deploy in one step.             |
| `precompress`   | `false`        | Precompress assets with gzip + brotli.                                                        |

## Limitations (v1)

- **SPA *and* prerendered both work.** `txco://static` resolves clean URLs and
  directory indexes (`/about` → `about.html`, `/blog` → `blog/index.html`, `/` →
  `index.html`), so prerendered routes serve their own HTML; the fallback op covers
  any remaining client-rendered routes. One edge: the fallback's `WHEN` matches
  extension-less paths, so a client route that ends in a dot-suffix (e.g. `/v1.2`)
  isn't caught — rare, and prerendering or an explicit op handles it.
- **Per-file size.** Assets are capped (10 MiB on the fleet path); typical
  SvelteKit chunks are far under this. A single very large asset needs a CDN.
- **Whole-stack deploy.** Each `txco apply` replaces the stack version; unchanged
  assets dedup by content hash, so the cost is metadata, not bytes.

## License

MIT
