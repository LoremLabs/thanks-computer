# @txco/svelte-adapter-thankscomputer

A [SvelteKit](https://svelte.dev/docs/kit) adapter for [Thanks, computer](https://www.thanks.computer)
(txco). It writes a **Web ABI build**:

- your app as `public/`;
- the ops a SvelteKit app needs as `ops/`;
- a `txco-web.json` manifest.

`txco apply` installs the build into a stack.

```sh
npm install -D @txco/svelte-adapter-thankscomputer
```

## Usage

```js
// svelte.config.js
import adapter from "@txco/svelte-adapter-thankscomputer";

export default {
  kit: {
    adapter: adapter({ out: "txco-web", fallback: "200.html" }),
  },
};
```

Bind the build to a stack in your workspace's `txco.yaml`. The path is
relative to the workspace root, and must lie outside `OPS/`:

```yaml
stacks:
  web:
    abi: www/txco-web
```

Then build, check and deploy:

```sh
vite build                      # writes www/txco-web/
txco web check www/txco-web     # installs it on a scratch chassis and probes it
txco apply                      # or: txco push web
```

## What it writes

```
txco-web/
  txco-web.json                   { "abi": 1, "immutable": ["<appDir>/immutable/"] }
  public/                         client assets + prerendered pages + the fallback shell
  ops/900000/spa-fallback.txcl    200 + the shell, for a navigation to a known page route
  ops/900000/spa-404.txcl         404 + the shell, for a navigation that matches no route
  ops/900900/not-found.txcl       a plain 404 for everything else (an asset miss, a POST)
```

The adapter wipes `out` and rewrites it on every build, so a stale op can't
survive.

- Add `out` to `.gitignore`. It's all build output, and the fallback ops embed
  the page shell, asset hashes included, so they change on every build. Build
  before you push: `txco` refuses a binding whose build is missing.
- `out` must be a directory of its own. The adapter refuses one inside `OPS/`
  or one holding anything else.

## How a request is answered

1. **A file in `public/`** is served by `txco://static` before your stack runs.
   That covers prerendered pages too: `/` → `index.html`, `/about` →
   `about.html`, `/blog` → `blog/index.html`.
2. **Your own ops** in the stack (APIs, auth, redirects) run next, at any scope
   below 900000.
3. **The build's ops** go last:
   - a GET or HEAD of a path that matches a **page route** gets the shell with
     200 — whatever dots the path carries (a param may hold one:
     `/p/mister.parade`), since the route table decides what a page is;
   - any other **navigation** (a GET or HEAD of a path with no extension) gets
     the shell with 404, and the client renders its error page;
   - everything else gets a plain 404, so every request is answered.

The route matcher comes from SvelteKit's own route table. If a route needs a
regex feature Go's RE2 lacks (lookaround, backreferences), every navigation
gets the shell with 200 instead.

## `appDir` and `_` paths

SvelteKit's default `appDir` is `_app`. txco treats a `_`-prefixed path as
private, except for files installed from a build's `public/`. The installer
marks those, so `_app/` and `_`-prefixed chunk hashes are served unchanged. An
`appDir` without `_` (`app`) works too.

## Options

| Option          | Default        | Meaning                                                                                                       |
| --------------- | -------------- | ------------------------------------------------------------------------------------------------------------- |
| `out`           | `"txco-web"`   | The Web ABI directory to write (outside `OPS/`).                                                              |
| `fallback`      | `"index.html"` | The SPA shell's file name in `public/`, or `false` for no SPA (navigations to unknown pages get a plain 404). |
| `fallbackOp`    | `true`         | Write the navigation ops.                                                                                     |
| `fallbackScope` | `900000`       | The navigation ops' scope; the catch-all goes 900 above it.                                                   |
| `apply`         | `false`        | Run `txco apply` after the build.                                                                             |
| `precompress`   | `false`        | Deprecated and ignored: the edge compresses.                                                                  |

## Moving from 0.2

0.2 wrote into the stack directory (`out: 'OPS/web'`).

1. Set `out` to a directory outside `OPS/` (e.g. `'txco-web'`).
2. Bind the stack in `txco.yaml`.
3. Delete the old `OPS/<stack>/FILES/` and the generated
   `OPS/<stack>/900000/spa-*.txcl`.

The adapter refuses an `out` inside `OPS/`, so a 0.2 config fails loudly
instead of losing your ops. The chassis must support Web ABI markers;
`txco apply` checks this for you.

## Limits

- **Static output only.** SSR (`+page.server.ts`, server `+server.ts`) needs a
  server runner, which no chassis has yet.
- **Dot paths** (`.well-known/…`) in the build never deploy; the adapter warns.

## License

MIT
