# @txco/nitro-preset

The `thanks-computer` [Nitro](https://nitro.build) preset. It builds a
Nitro app ([Nuxt](https://nuxt.com), [Analog](https://analogjs.org),
[SolidStart](https://start.solidjs.com)) as a
**Web ABI build** for [Thanks, Computer](https://www.thanks.computer) (txco):

- your site as `public/`;
- the ops it needs as `ops/`;
- its server as `server/` (`nuxt build` only);
- a `txco-web.json` manifest.

`txco apply` installs the build into a stack.

```sh
npm install -D @txco/nitro-preset
```

It works with Nitro 2 (`nitropack`), which is what Nuxt 4, Analog 2 and
SolidStart 1 run on.

## Nuxt

```ts
// nuxt.config.ts
export default defineNuxtConfig({
  nitro: { preset: "@txco/nitro-preset" },
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
nuxt generate                  # writes txco-web/
txco web check txco-web        # installs it on a scratch chassis and probes it
txco apply                     # or: txco push web
```

Gitignore `txco-web/`. It's all build output, and the 404 op embeds the page,
asset hashes included, so it changes on every build.

## Analog

Analog sets Nitro's output paths itself, so set all three:

```ts
// vite.config.ts
import { resolve } from "node:path";

const out = resolve(__dirname, "txco-web");

export default defineConfig({
  plugins: [
    analog({
      static: true,
      prerender: { routes: ["/", "/about"] },
      nitro: {
        preset: "@txco/nitro-preset",
        output: { dir: out, publicDir: `${out}/public`, serverDir: `${out}/server` },
        thanksComputer: { immutable: ["assets/"] },
      },
    }),
  ],
});
```

Analog doesn't mark its hashed assets for caching, so name the directory with
`immutable` (above) to get the year-long cache.

## SolidStart

**SolidStart 1** builds through vinxi on Nitro 2, so the preset goes in
`app.config.ts`:

```ts
// app.config.ts
import { defineConfig } from "@solidjs/start/config";

export default defineConfig({
  server: {
    preset: "@txco/nitro-preset",
    static: true,
    prerender: { crawlLinks: true },
    // @ts-expect-error: the preset's own option, not in vinxi's types
    thanksComputer: { immutable: ["_build/assets/"] },
  },
});
```

**SolidStart 2** runs on Nitro 3, which doesn't load this preset. Until a
built-in Nitro 3 preset exists, SolidStart's Nitro 2 bridge
(`@solidjs/vite-plugin-nitro-2`, deprecated upstream) takes the same
options:

```ts
// vite.config.ts
import { solidStart } from "@solidjs/start/config";
import { nitroV2Plugin } from "@solidjs/vite-plugin-nitro-2";

export default defineConfig({
  plugins: [
    solidStart(),
    nitroV2Plugin({
      preset: "@txco/nitro-preset",
      static: true,
      prerender: { routes: ["/"], crawlLinks: true },
      // @ts-expect-error: the preset's own option
      thanksComputer: { immutable: ["_build/assets/"] },
    }),
  ],
});
```

SolidStart doesn't mark `_build/assets/` for caching, so `immutable` names it.
For `ssr: false`, add `thanksComputer: { spa: true }`; SolidStart's SPA build
isn't detected on its own.

## What it writes

```
txco-web/
  txco-web.json              { "abi": 1, "immutable": ["_nuxt/"], … }
  public/                    Nitro's public output, prerendered pages included
  ops/900000/page-404.txcl   404 + 404.html, for a navigation to a page that doesn't exist
  ops/900900/not-found.txcl  a plain 404 for everything else (an asset miss, a POST)
  server/index.mjs           nuxt build only: the app as a Fetch handler
```

How a navigation that no page answers is handled depends on the build:

- **A prerendered site** (`nuxt generate`) gets `404.html` with status 404.
  Nuxt's `404.html` is its app shell, so a client route that wasn't
  prerendered still renders. For a server-rendered error page instead, set
  `experimental: { prerenderErrorPages: true }` in `nuxt.config`.
- **An `ssr: false` app** gets the shell (`200.html`) with status 200, since
  the client router renders every page. It's detected for Nuxt and Analog; set
  `thanksComputer: { spa: true }` (or `false`) to force it.
- **A server build** (`nuxt build`) gets the 404 op too, for a static-only
  install.

`compressPublicAssets`' `.gz`/`.br` copies, and a bundler's `.vite/`
metadata, are left out of `public/`: nothing serves them, and the edge
compresses.

`_` paths (`_nuxt/`, `_payload.json`) are served: the installer marks every
`_` path in `public/` as public.

## The server

`nuxt build` adds `server/index.mjs`. It exports
`{ fetch(request, ctx) }`, and the client's IP from `ctx.client.ip` reaches
the app through h3's `getRequestIP`. Every dependency is bundled in, so the
server needs no install step. A native addon (`sharp`, say) can't be bundled.

No chassis runs `server/` yet. `txco apply` and `txco web check` refuse a
build with a server unless you pass `--static-only`, which deploys
`public/` and `ops/` and leaves the server out:

```sh
nuxt build
npx -p @txco/web-abi txco-web-abi check-server txco-web   # the server half
txco web check txco-web --static-only    # the static half
txco apply --static-only
```

## Options

Set them as `nitro: { thanksComputer: { … } }` (Nuxt), or as `thanksComputer`
in a Nitro config.

| Option      | Default        | Meaning |
| ----------- | -------------- | ------- |
| `spa`       | detected       | Answer every unknown page path with the shell and 200. |
| `immutable` | derived        | The `public/` prefixes cached for a year (`["assets/"]`). For Nuxt it's derived as `_nuxt/`. |
| `scope`     | `900000`       | The navigation op's scope. The catch-all goes 900 above it. |

For the types in `nuxt.config.ts`, add `import type {} from "@txco/nitro-preset"`.

## Safety

Nitro empties its output directory on every build. Before that happens, the
preset refuses an output directory that:

- is inside `OPS/`;
- holds anything that isn't part of a Web ABI build;
- holds your sources.

A refused build leaves the directory untouched.

## Limits

- **Nitro 2 only.** Nitro 3 doesn't load external presets; a built-in
  `thanks-computer` preset there is the plan.
- **Dot paths** (`.well-known/…`) in `public/` never deploy; the preset warns.
- **A non-root `app.baseURL`** is untested.

## License

MIT
