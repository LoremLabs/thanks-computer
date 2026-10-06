// Real `vite build`s of small fixtures, each in a temp copy, with the plugin.
import { test } from "node:test";
import assert from "node:assert/strict";
import { cp, mkdtemp, mkdir, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { build } from "vite";
import { readManifest } from "@txco/web-abi/manifest";

import txco, { chooseMode, publicPrefix } from "../dist/index.js";

const fixtures = new URL("./fixtures/", import.meta.url).pathname;
const exists = (p) => stat(p).then(() => true, () => false);

async function project(name) {
  const root = join(await mkdtemp(join(tmpdir(), "vite-plugin-")), name);
  await cp(join(fixtures, name), root, { recursive: true });
  return { root, cleanup: () => rm(join(root, ".."), { recursive: true, force: true }) };
}

const viteBuild = (root, config = {}, opts) =>
  build({ root, configFile: false, logLevel: "silent", plugins: [txco(opts)], ...config });

test("a single-page app: the SPA fallback serves index.html; dist/ stays; assets/ is immutable", async () => {
  const { root, cleanup } = await project("spa");
  try {
    await viteBuild(root);
    const out = join(root, "txco-web");
    assert.deepEqual((await readdir(out)).sort(), ["ops", "public", "txco-web.json"]);
    assert.ok(await exists(join(root, "dist/index.html")), "dist/ is left alone");
    const m = await readManifest(out); // the kit's validator
    assert.deepEqual(m.immutable, ["assets/"]);
    assert.equal(m["x-vite"].mode, "spa");
    assert.equal(m["x-producer"].name, "@txco/vite-plugin");
    assert.deepEqual((await readdir(join(out, "ops/900000"))), ["spa-fallback.txcl"]);
    const op = await readFile(join(out, "ops/900000/spa-fallback.txcl"), "utf8");
    assert.match(op, /app shell \(index\.html\) with 200/);
    assert.ok(op.includes("<div id=\\\"app\\\">"), "index.html embedded");
    assert.ok(await exists(join(out, "public/robots.txt")), "public/ files at the root");
    assert.ok(!(await exists(join(out, "public/.well-known"))), "dot paths dropped");
    const assets = await readdir(join(out, "public/assets"));
    assert.ok(assets.some((f) => /^index-[\w-]{8}\.js$/.test(f)), assets.join(","));
  } finally {
    await cleanup();
  }
});

test("a multi-page app: unknown pages get 404.html with 404", async () => {
  const { root, cleanup } = await project("mpa");
  try {
    await viteBuild(root, { build: { rollupOptions: { input: { main: join(root, "index.html"), about: join(root, "about/index.html") } } } });
    const out = join(root, "txco-web");
    const m = await readManifest(out);
    assert.deepEqual([m["x-vite"].mode, m["x-vite"].pages], ["mpa", 2]);
    const op = await readFile(join(out, "ops/900000/page-404.txcl"), "utf8");
    assert.match(op, /Serves 404\.html with 404/);
    assert.ok(op.includes("no such page"));
    assert.ok(await exists(join(out, "public/about/index.html")));
  } finally {
    await cleanup();
  }
});

test("base: /app/ puts the site under public/app/", async () => {
  const { root, cleanup } = await project("spa");
  try {
    await viteBuild(root, { base: "/app/" });
    const out = join(root, "txco-web");
    assert.ok(await exists(join(out, "public/app/index.html")));
    assert.ok(await exists(join(out, "public/app/robots.txt")));
    assert.deepEqual((await readManifest(out)).immutable, ["app/assets/"]);
  } finally {
    await cleanup();
  }
});

test("the options: spa forced off, an explicit immutable list, a scope", async () => {
  const { root, cleanup } = await project("spa");
  try {
    await viteBuild(root, {}, { spa: false, immutable: ["assets/"], scope: 950000 });
    const out = join(root, "txco-web");
    assert.deepEqual((await readdir(join(out, "ops"))).sort(), ["950000", "950900"]);
    assert.match(await readFile(join(out, "ops/950000/page-404.txcl"), "utf8"), /plain 404 page \(the build has no 404\.html\)/);
  } finally {
    await cleanup();
  }
});

test("library and SSR builds write nothing", async () => {
  const { root, cleanup } = await project("spa");
  try {
    await viteBuild(root, { build: { lib: { entry: join(root, "src/main.js"), formats: ["es"], fileName: "lib" }, outDir: "lib-dist" } });
    await viteBuild(root, { build: { ssr: join(root, "src/main.js"), outDir: "ssr-dist" } });
    assert.ok(!(await exists(join(root, "txco-web"))));
  } finally {
    await cleanup();
  }
});

test("the guard: foreign files in the output dir, or an output dir inside dist/, refuse the build", async () => {
  const { root, cleanup } = await project("spa");
  try {
    await mkdir(join(root, "txco-web"), { recursive: true });
    await writeFile(join(root, "txco-web/notes.md"), "keep me");
    await assert.rejects(viteBuild(root), /"notes\.md"/);
    assert.equal(await readFile(join(root, "txco-web/notes.md"), "utf8"), "keep me");
    await assert.rejects(viteBuild(root, {}, { out: "dist/txco-web" }), /inside .*dist/);
    await assert.rejects(viteBuild(root, {}, { out: "OPS/web" }), /inside OPS\//);
  } finally {
    await cleanup();
  }
});

test("chooseMode and publicPrefix", () => {
  assert.equal(chooseMode(["index.html"]), "spa");
  assert.equal(chooseMode(["about/index.html", "index.html"]), "mpa");
  assert.equal(chooseMode(["a.html", "b.html"], true), "spa");
  assert.deepEqual(publicPrefix("/"), { prefix: "", external: false });
  assert.deepEqual(publicPrefix("./"), { prefix: "", external: false });
  assert.deepEqual(publicPrefix(""), { prefix: "", external: false });
  assert.deepEqual(publicPrefix("/app/"), { prefix: "app/", external: false });
  assert.deepEqual(publicPrefix("/a/b/"), { prefix: "a/b/", external: false });
  assert.deepEqual(publicPrefix("https://cdn.example.com/x/"), { prefix: "", external: true });
});
