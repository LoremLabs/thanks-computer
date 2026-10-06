# feat: add `thanks-computer` preset

Adds `thanks-computer` and `thanks-computer-static` presets for [Thanks, Computer](https://www.thanks.computer).

A build is a Web ABI directory, `txco-web/`:

- `public/`: the static output. The platform serves it before the app runs (`serveStatic: false`).
- `server/index.mjs`: the app as `export default { fetch(request, ctx) }`, with every dependency bundled (`noExternals`). `ctx.client.ip` becomes `req.ip`.
- `ops/`: two small generated rules the platform runs. A page navigation nothing answered gets `404.html` with 404, or the app shell with 200 for an app rendered in the browser. Everything else gets a plain 404.
- `txco-web.json`: the manifest. `immutable` is derived from `publicAssets` entries with a one-year `maxAge` (`_nuxt/` for Nuxt).

`thanks-computer-static` extends `static`, so a prerendered site crawls from `/`, the same way `zeabur-static` does. `nitro.json` stays where it is, for `nitro preview` and `nitro deploy`. `commands.deploy` is `txco apply`.

Changes:

- `src/presets/thanks-computer/` (preset, runtime, types, utils)
- `docs/2.deploy/20.providers/thanks-computer.md`
- `test/presets/thanks-computer.test.ts`
- the `_all.gen.ts` and `_types.gen.ts` entries

## Verification

Checked against `nitro@3.0.260903-beta`, loading these files as an external preset:

- **`nitro build --preset thanks-computer`:**
  - the server entry passes the Web ABI conformance kit (`@txco/web-abi check-server`: load, `GET /` → 200, unknown page → 404, POST → 404, HEAD → 200);
  - `ctx.client.ip` reaches h3's `getRequestIP`;
  - `txco web check --static-only` passes.
- **`nitro build --preset thanks-computer-static`:** prerenders `/` and `/about` (crawled), and `txco web check --strict` passes.

The same layout ships today for Nitro 2 (Nuxt 4, Analog 2) as [`@txco/nitro-preset`](https://www.npmjs.com/package/@txco/nitro-preset), checked with Nuxt 4.6 (`generate`, `ssr: false`, `build`) and Analog 2.8.

Still to run in this branch: `pnpm gen-presets`, to confirm the hand-written `.gen.ts` entries, and `pnpm vitest test/presets/thanks-computer.test.ts`.
