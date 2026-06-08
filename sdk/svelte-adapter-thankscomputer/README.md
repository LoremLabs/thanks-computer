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
vite build       # writes OPS/web/FILES/** and OPS/web/100/spa-fallback.txcl
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
- Extension-less requests (`/`, `/dashboard`, …) have no matching file, so static
  falls through. The generated `spa-fallback.txcl` op then serves the app shell
  for them, so deep links and reloads render correctly.

## Options

| Option          | Default        | Description                                                                                  |
| --------------- | -------------- | -------------------------------------------------------------------------------------------- |
| `out`           | `"OPS/web"`    | Stack directory holding `FILES/` and scope folders; build lands in `<out>/FILES/`.           |
| `fallback`      | `"index.html"` | SPA fallback page filename, or `false` to disable SPA mode (serve only prerendered files).   |
| `fallbackOp`    | `true`         | Generate `<out>/<fallbackScope>/spa-fallback.txcl` to serve the shell for client routes.     |
| `fallbackScope` | `100`          | txcl scope for the generated fallback op.                                                     |
| `apply`         | `false`        | Run `txco apply` in the current directory after building, to deploy in one step.             |
| `precompress`   | `false`        | Precompress assets with gzip + brotli.                                                        |

## Limitations (v1)

- **SPA mode first.** The fallback op serves a single shell for all client
  routes. Fully **prerendered** multi-page sites need directory-index resolution
  in `txco://static` (planned) — until then, extension-less prerendered routes
  fall back to the shell rather than their own HTML.
- **Per-file size.** Assets are capped (10 MiB on the fleet path); typical
  SvelteKit chunks are far under this. A single very large asset needs a CDN.
- **Whole-stack deploy.** Each `txco apply` replaces the stack version; unchanged
  assets dedup by content hash, so the cost is metadata, not bytes.

## License

MIT
