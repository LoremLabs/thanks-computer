// writeBuild against a fake finished build in a temp dir; the preset's guard.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { readManifest } from "@txco/web-abi/manifest";

import { writeBuild } from "../dist/build.js";
import txco from "../dist/index.js";
import { APP } from "./routes.test.mjs";

const exists = (p) => stat(p).then(() => true, () => false);
const SHELL = '<!doctype html><html><body><div id="root"></div></body></html>';

async function project(files) {
  const root = await mkdtemp(join(tmpdir(), "rr-preset-"));
  for (const [rel, text] of Object.entries(files)) {
    await mkdir(join(root, rel, ".."), { recursive: true });
    await writeFile(join(root, rel), text);
  }
  const warnings = [];
  const input = (over = {}) => ({
    root,
    buildDirectory: "build",
    base: "/",
    assetsDir: "assets",
    basename: "/",
    ssr: false,
    routes: APP,
    options: {},
    log: { info: () => {}, warn: (m) => warnings.push(m) },
    ...over,
  });
  return { root, input, warnings, cleanup: () => rm(root, { recursive: true, force: true }) };
}

const SPA = {
  "build/client/index.html": SHELL,
  "build/client/assets/entry.client-BxYz12Ab.js": "x",
  "build/client/favicon.ico": "i",
  "build/client/.vite/manifest.json": "{}",
};

test("SPA mode: the route-aware pair serves index.html; assets/ is immutable", async () => {
  const p = await project(SPA);
  try {
    const r = await writeBuild(p.input());
    assert.equal(r.mode, "routes");
    assert.deepEqual(r.ops, ["900000/spa-404.txcl", "900000/spa-fallback.txcl", "900900/not-found.txcl"]);
    const out = join(p.root, "txco-web");
    const m = await readManifest(out);
    assert.deepEqual(m.immutable, ["assets/"]);
    assert.deepEqual(m["x-react-router"], { ssr: false, mode: "routes", routes: Object.keys(APP).length });
    assert.ok(await exists(join(out, "public/favicon.ico")));
    assert.ok(!(await exists(join(out, "public/.vite"))), ".vite/ left out");
    assert.deepEqual(p.warnings, [], ".vite/ isn't warned about");
    assert.match(await readFile(join(out, "ops/900000/spa-fallback.txcl"), "utf8"), /=~ \/\^\(\?:/);
  } finally {
    await p.cleanup();
  }
});

test("a prerendered home makes __spa-fallback.html the shell", async () => {
  const p = await project({ ...SPA, "build/client/__spa-fallback.html": "<!doctype html><title>shell</title>", "build/client/_root.data": "{}" });
  try {
    await writeBuild(p.input());
    const op = await readFile(join(p.root, "txco-web/ops/900000/spa-fallback.txcl"), "utf8");
    assert.match(op, /app shell \(__spa-fallback\.html\)/);
    assert.ok(await exists(join(p.root, "txco-web/public/_root.data")));
  } finally {
    await p.cleanup();
  }
});

test("routeAware: false, and a route that can't translate, give the plain fallback", async () => {
  const p = await project(SPA);
  try {
    assert.equal((await writeBuild(p.input({ options: { routeAware: false } }))).mode, "spa");
    assert.equal((await writeBuild(p.input({ routes: { x: { id: "x", path: "file.:ext" } } }))).mode, "spa");
  } finally {
    await p.cleanup();
  }
});

test("ssr: true deploys the prerendered pages with the 404 page, and warns", async () => {
  const p = await project({ "build/client/about/index.html": "<h1>about</h1>", "build/client/assets/a-BxYz12Ab.js": "x", "build/server/index.js": "" });
  try {
    const r = await writeBuild(p.input({ ssr: true }));
    assert.equal(r.mode, "404");
    assert.equal((await readManifest(join(p.root, "txco-web"))).server, undefined);
    assert.ok(p.warnings.some((w) => /ssr: true needs a server/.test(w)));
  } finally {
    await p.cleanup();
  }
});

test("base: /app/ puts the build under public/app/", async () => {
  const p = await project(SPA);
  try {
    await writeBuild(p.input({ base: "/app/", basename: "/app/" }));
    assert.ok(await exists(join(p.root, "txco-web/public/app/index.html")));
    assert.deepEqual((await readManifest(join(p.root, "txco-web"))).immutable, ["app/assets/"]);
  } finally {
    await p.cleanup();
  }
});

test("the preset: buildEnd from reactRouterConfig(); the guard refuses before the build", async () => {
  const p = await project({ "txco-web/notes.md": "keep me" });
  const cwd = process.cwd();
  try {
    process.chdir(p.root);
    const preset = txco();
    assert.equal(preset.name, "@txco/react-router");
    assert.equal(typeof preset.reactRouterConfig({ reactRouterUserConfig: {} }).buildEnd, "function");
    await assert.rejects(preset.reactRouterConfigResolved({ reactRouterConfig: { buildDirectory: "build" } }), /"notes\.md"/);
    await assert.rejects(txco({ out: "build/txco-web" }).reactRouterConfigResolved({ reactRouterConfig: { buildDirectory: "build" } }), /inside .*build/);
    assert.equal(await readFile(join(p.root, "txco-web/notes.md"), "utf8"), "keep me");
  } finally {
    process.chdir(cwd);
    await p.cleanup();
  }
});
