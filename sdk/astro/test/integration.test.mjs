// The integration's two hooks against a fake finished build in a temp dir.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

import { readManifest } from "@txco/web-abi/manifest";

import txco from "../dist/index.js";

const exists = (p) => stat(p).then(() => true, () => false);
const dirURL = (p) => pathToFileURL(p.endsWith("/") ? p : p + "/");

async function site(files, { base = "/", output = "static", outDir = "dist" } = {}) {
  const root = await mkdtemp(join(tmpdir(), "astro-int-"));
  for (const [rel, text] of Object.entries(files)) {
    await mkdir(join(root, rel, ".."), { recursive: true });
    await writeFile(join(root, rel), text);
  }
  const logs = [];
  const logger = { info: (m) => logs.push(m), warn: (m) => logs.push(`warn: ${m}`) };
  const config = {
    root: dirURL(root),
    srcDir: dirURL(join(root, "src")),
    publicDir: dirURL(join(root, "public")),
    outDir: dirURL(join(root, outDir)),
    base,
    output,
    build: { assets: "_astro" },
  };
  return { root, config, logger, logs, cleanup: () => rm(root, { recursive: true, force: true }) };
}

async function run(s, opts, dir = "dist") {
  const it = txco(opts);
  await it.hooks["astro:config:done"]({ config: s.config, buildOutput: s.config.output, logger: s.logger });
  await it.hooks["astro:build:done"]({ dir: dirURL(join(s.root, dir)), pages: [{ pathname: "" }, { pathname: "about/" }], logger: s.logger });
  return join(s.root, "txco-web");
}

const STATIC = {
  "dist/index.html": "<h1>home</h1>",
  "dist/about/index.html": "<h1>about</h1>",
  "dist/404.html": "<!doctype html><h1>no such page</h1>",
  "dist/_astro/index.BxYz12Ab.css": "x",
  "dist/_astro/hoisted.CdEf34Gh.js": "y",
  "dist/favicon.svg": "s",
};

test("a static site: the 404-page op serves 404.html; _astro/ is immutable", async () => {
  const s = await site(STATIC);
  try {
    const out = await run(s);
    const m = await readManifest(out);
    assert.deepEqual(m.immutable, ["_astro/"]);
    assert.deepEqual(m["x-astro"], { output: "static", pages: 2 });
    const op = await readFile(join(out, "ops/900000/page-404.txcl"), "utf8");
    assert.match(op, /Serves 404\.html with 404/);
    assert.ok(op.includes("no such page"));
    assert.ok(await exists(join(out, "public/about/index.html")));
    assert.ok(await exists(join(out, "public/_astro/hoisted.CdEf34Gh.js")));
    assert.ok(!s.logs.some((l) => l.startsWith("warn:")), s.logs.join("\n"));
  } finally {
    await s.cleanup();
  }
});

test("no 404.html: the built-in 404 page, and a note to add one", async () => {
  const s = await site({ "dist/index.html": "<h1>home</h1>", "dist/about.html": "<h1>about</h1>" });
  try {
    const out = await run(s);
    const op = await readFile(join(out, "ops/900000/page-404.txcl"), "utf8");
    assert.match(op, /plain 404 page \(the build has no 404\.html\)/);
    assert.match(op, /src\/pages\/404\.astro/);
    assert.equal((await readManifest(out)).immutable, undefined, "no _astro/, no immutable prefix");
  } finally {
    await s.cleanup();
  }
});

test("base: /docs puts the site under public/docs/", async () => {
  const s = await site(STATIC, { base: "/docs" });
  try {
    const out = await run(s);
    assert.ok(await exists(join(out, "public/docs/index.html")));
    assert.deepEqual((await readManifest(out)).immutable, ["docs/_astro/"]);
  } finally {
    await s.cleanup();
  }
});

test("a server build deploys build.client's prerendered pages, and warns", async () => {
  const s = await site({ "dist/client/about/index.html": "<h1>about</h1>", "dist/server/entry.mjs": "" }, { output: "server" });
  try {
    const out = await run(s, {}, "dist/client");
    assert.ok(await exists(join(out, "public/about/index.html")));
    assert.ok(!(await exists(join(out, "public/entry.mjs"))));
    assert.ok(s.logs.some((l) => /output: 'server' needs a server/.test(l)));
  } finally {
    await s.cleanup();
  }
});

test("the guard: foreign files, an out inside dist/ or OPS/, refuse before the build", async () => {
  const s = await site({ "txco-web/notes.md": "keep me" });
  try {
    const hook = (opts) => txco(opts).hooks["astro:config:done"]({ config: s.config, buildOutput: "static", logger: s.logger });
    await assert.rejects(hook(), /"notes\.md"/);
    await assert.rejects(hook({ out: "dist/txco-web" }), /inside .*dist/);
    await assert.rejects(hook({ out: "OPS/web" }), /inside OPS\//);
    assert.equal(await readFile(join(s.root, "txco-web/notes.md"), "utf8"), "keep me");
  } finally {
    await s.cleanup();
  }
});
