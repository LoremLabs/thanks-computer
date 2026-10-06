// finalize against a fake Nitro over a temp dir: no framework install needed.
// The real builds (Nuxt, Analog) are checked by hand with txco web check.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

import { readManifest } from "@txco/web-abi/manifest";

import { finalize, isSpa } from "../dist/finalize.js";
import { YEAR } from "../dist/immutable.js";

const SHELL = '<!doctype html><html><body><div id="__nuxt"></div></body></html>';
const NUXT_ASSETS = [
  { baseURL: "/", maxAge: 0 },
  { baseURL: "/_nuxt", maxAge: YEAR },
  { baseURL: "/_nuxt/builds/meta", maxAge: YEAR },
  { baseURL: "/_nuxt/builds", maxAge: 1 },
];

async function build(files, { static: isStatic = true, framework = { name: "nuxt", version: "4.6.0" }, ...rest } = {}) {
  const dir = join(await mkdtemp(join(tmpdir(), "nitro-preset-")), "txco-web");
  for (const [rel, text] of Object.entries(files)) {
    await mkdir(dirname(join(dir, rel)), { recursive: true });
    if (text !== null) await writeFile(join(dir, rel), text);
  }
  const logs = [];
  const nitro = {
    options: {
      static: isStatic,
      baseURL: "/",
      output: { dir, publicDir: join(dir, "public"), serverDir: join(dir, "server") },
      publicAssets: NUXT_ASSETS,
      routeRules: {},
      framework,
      ...rest.options,
    },
    _prerenderedRoutes: rest.prerendered ?? [],
    logger: { info: (m) => logs.push(m), warn: (m) => logs.push(m), success: (m) => logs.push(m) },
  };
  return { dir, nitro, logs, cleanup: () => rm(dirname(dir), { recursive: true, force: true }) };
}

const exists = (p) => stat(p).then(() => true, () => false);

test("a generated Nuxt site: a 404-page op, the catch-all, _nuxt/ immutable, nitro.json gone", async () => {
  const b = await build({
    "nitro.json": JSON.stringify({ versions: { nitro: "2.13.4" } }),
    "public/index.html": "<h1>home</h1>",
    "public/200.html": SHELL,
    "public/404.html": SHELL,
    "public/about/_payload.json": "{}",
    "public/_nuxt/BxYz12Ab.js": "x",
    "public/_nuxt/builds/latest.json": "{}",
    "public/.well-known/thing": "x",
  });
  try {
    const r = await finalize(b.nitro, { server: false });
    assert.equal(r.mode, "static");
    assert.deepEqual(r.ops, ["900000/page-404.txcl", "900900/not-found.txcl"]);
    assert.ok(!(await exists(join(b.dir, "nitro.json"))), "nitro.json removed");
    assert.ok(!(await exists(join(b.dir, "server"))));
    const m = await readManifest(b.dir); // the kit's validator
    assert.deepEqual(m.immutable, ["_nuxt/"]);
    assert.equal(m.server, undefined);
    assert.deepEqual(m["x-nitro"], { nitro: "2.13.4", preset: "thanks-computer", framework: { name: "nuxt", version: "4.6.0" }, mode: "static" });
    assert.equal(m["x-producer"].name, "@txco/nitro-preset");
    const op = await readFile(join(b.dir, "ops/900000/page-404.txcl"), "utf8");
    assert.match(op, /Serves 404\.html with 404/);
    assert.ok(op.includes(SHELL.replace(/"/g, '\\"')), "404.html embedded");
    // A dot path never deploys; latest.json isn't flagged as unhashed.
    assert.equal(r.warnings.length, 1, r.warnings.join("\n"));
    assert.match(r.warnings[0], /dot path/);
    assert.deepEqual((await readdir(b.dir)).sort(), ["ops", "public", "txco-web.json"]);
  } finally {
    await b.cleanup();
  }
});

test("an ssr: false Nuxt app: the SPA fallback serves 200.html", async () => {
  const b = await build({ "public/index.html": SHELL, "public/200.html": SHELL, "public/404.html": SHELL }, { prerendered: [{ route: "/index.html" }, { route: "/200.html" }] });
  try {
    const r = await finalize(b.nitro, { server: false });
    assert.equal(r.mode, "spa");
    assert.deepEqual(r.ops, ["900000/spa-fallback.txcl", "900900/not-found.txcl"]);
    assert.match(await readFile(join(b.dir, "ops/900000/spa-fallback.txcl"), "utf8"), /app shell \(200\.html\) with 200/);
  } finally {
    await b.cleanup();
  }
});

test("isSpa: the option wins; Nuxt's islands routeRule and Analog's client renderer count", () => {
  const n = (options, prerendered = []) => ({ options: { routeRules: {}, ...options }, _prerenderedRoutes: prerendered });
  assert.equal(isSpa(n({})), false);
  assert.equal(isSpa(n({ routeRules: { "/**": { ssr: false } } })), true);
  assert.equal(isSpa(n({ renderer: "#ANALOG_CLIENT_RENDERER" })), true);
  assert.equal(isSpa(n({ renderer: "#ANALOG_SSR_RENDERER" })), false);
  assert.equal(isSpa(n({ thanksComputer: { spa: false } }, [{ route: "/index.html" }])), false);
  assert.equal(isSpa(n({ thanksComputer: { spa: true } })), true);
});

test("a server build: server.entry in the manifest, the 404 op without a 404.html", async () => {
  const b = await build({ "server/index.mjs": "export default {}", "server/chunks/a.mjs": "", "public/_nuxt/BxYz12Ab.js": "x" }, { static: false });
  try {
    const r = await finalize(b.nitro, { server: true });
    assert.equal(r.mode, "server");
    const m = await readManifest(b.dir);
    assert.deepEqual(m.server, { entry: "server/index.mjs" });
    const op = await readFile(join(b.dir, "ops/900000/page-404.txcl"), "utf8");
    assert.match(op, /no 404\.html/);
    assert.match(op, /dispatch op replaces this one/);
  } finally {
    await b.cleanup();
  }
});

test("a server build without its entry fails", async () => {
  const b = await build({ "server/other.mjs": "" }, { static: false });
  try {
    await assert.rejects(finalize(b.nitro, { server: true }), /no entry at/);
  } finally {
    await b.cleanup();
  }
});

test("Analog's static build: the empty server/ goes; the immutable option and its unhashed warning", async () => {
  const b = await build(
    { "server/": null, "public/index.html": SHELL, "public/assets/main-AbCd12Ef.js": "x", "public/assets/logo.svg": "x" },
    { framework: { name: "analog", version: "2.8.0" }, options: { publicAssets: [{ dir: "/x" }], renderer: "#ANALOG_CLIENT_RENDERER", thanksComputer: { immutable: ["assets/"] } } },
  );
  await mkdir(join(b.dir, "server"), { recursive: true });
  try {
    const r = await finalize(b.nitro, { server: false });
    assert.equal(r.mode, "spa", "index.html is the shell when there's no 200.html");
    assert.ok(!(await exists(join(b.dir, "server"))), "empty server/ removed");
    assert.deepEqual((await readManifest(b.dir)).immutable, ["assets/"]);
    assert.ok(r.warnings.some((w) => /carry no content hash \(assets\/logo\.svg\)/.test(w)), r.warnings.join("\n"));
  } finally {
    await b.cleanup();
  }
});

test("a static build's leftover server/ (the prerenderer's bundle) is removed; a bad immutable option fails", async () => {
  const b = await build({ "server/index.mjs": "", "server/chunks/x.mjs": "", "public/404.html": SHELL });
  try {
    const r = await finalize(b.nitro, { server: false });
    assert.equal(r.mode, "static");
    assert.ok(!(await exists(join(b.dir, "server"))), "server/ removed");
    assert.equal((await readManifest(b.dir)).server, undefined);
    b.nitro.options.thanksComputer = { immutable: ["/assets"] };
    await assert.rejects(finalize(b.nitro, { server: false }), /thanksComputer\.immutable/);
  } finally {
    await b.cleanup();
  }
});

test("vinxi's leftovers: .vite/ under public/ and compressPublicAssets' .gz/.br copies are left out", async () => {
  const b = await build(
    {
      "public/index.html": "<h1>home</h1>",
      "public/404.html": SHELL,
      "public/_build/.vite/manifest.json": "{}",
      "public/_build/assets/client-xTzCoZPI.js": "x",
      "public/_build/assets/client-xTzCoZPI.js.br": "b",
      "public/_build/assets/client-xTzCoZPI.js.gz": "g",
      "public/data.json.gz": "a user's own archive",
    },
    { options: { compressPublicAssets: true, thanksComputer: { immutable: ["_build/assets/"] } } },
  );
  try {
    const r = await finalize(b.nitro, { server: false });
    assert.ok(!(await exists(join(b.dir, "public/_build/.vite"))), ".vite/ removed");
    assert.ok(!(await exists(join(b.dir, "public/_build/assets/client-xTzCoZPI.js.br"))));
    assert.ok(!(await exists(join(b.dir, "public/_build/assets/client-xTzCoZPI.js.gz"))));
    assert.ok(await exists(join(b.dir, "public/data.json.gz")), "a .gz with no sibling is the user's");
    assert.deepEqual((await readManifest(b.dir)).immutable, ["_build/assets/"]);
    assert.deepEqual(r.warnings, [], r.warnings.join("\n"));
  } finally {
    await b.cleanup();
  }
});
