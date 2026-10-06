// The shared producer helpers. The ops are golden-tested (UPDATE_GOLDEN=1
// rewrites them; review the diff); chassis/webabi strict-parses the same
// goldens with txcl and checks every producer's agree.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  BUILTIN_404,
  catchAllScope,
  checkOutDir,
  copyPublic,
  publicPrefix,
  listFiles,
  looksHashed,
  renderOps,
  writeManifest,
  writeOps,
} from "../dist/producer.js";

const golden = new URL("./golden/", import.meta.url).pathname;
const SHELL = '<!doctype html><html><head><script type="module" src="/assets/index-BxYz12Ab.js"></script></head><body><div id="app">\\o/</div></body></html>';
const base = { producer: "@txco/web-abi" };

const cases = {
  spa: { ...base, mode: "spa", page: { name: "index.html", html: SHELL }, notes: ["A producer's own note."] },
  "page-404": { ...base, mode: "404", page: { name: "404.html", html: SHELL } },
  "builtin-404": { ...base, mode: "404", page: null },
  routes: { ...base, mode: "routes", page: { name: "200.html", html: SHELL }, routes: "^(?:(?:\\/)|(?:\\/book\\/([^\\/]+?)\\/?))$" },
  none: { ...base, mode: "none", page: null },
};

async function check(name, files) {
  const dir = join(golden, name);
  if (process.env.UPDATE_GOLDEN) {
    await rm(dir, { recursive: true, force: true });
    for (const [rel, text] of Object.entries(files)) {
      await mkdir(join(dir, rel, ".."), { recursive: true });
      await writeFile(join(dir, rel), text);
    }
    return;
  }
  for (const [rel, text] of Object.entries(files)) {
    assert.equal(text, await readFile(join(dir, rel), "utf8"), `${name}/${rel}`);
  }
  const want = [];
  for (const scope of await readdir(dir)) for (const f of await readdir(join(dir, scope))) want.push(`${scope}/${f}`);
  assert.deepEqual(Object.keys(files).sort(), want.sort(), `${name}: the set of files`);
}

for (const [name, opts] of Object.entries(cases)) {
  test(`renderOps: ${name}`, async () => check(name, renderOps(opts)));
}

test("renderOps: an spa build needs its shell; the scope moves both ops", () => {
  assert.throws(() => renderOps({ ...base, mode: "spa", page: null }), /needs its shell/);
  assert.throws(() => renderOps({ ...base, mode: "routes", page: cases.spa.page }), /route matcher/);
  assert.equal(catchAllScope(900000), 900900);
  assert.deepEqual(Object.keys(renderOps({ ...base, mode: "404", page: null, scope: 950000 })).sort(), ["950000/page-404.txcl", "950900/not-found.txcl"]);
  assert.ok(renderOps(cases["builtin-404"])["900000/page-404.txcl"].includes("404 Not Found"));
  assert.ok(BUILTIN_404.startsWith("<!doctype html>"));
});

test("renderOps: quotes and backslashes in the page are escaped", () => {
  const body = renderOps(cases["page-404"])["900000/page-404.txcl"];
  assert.ok(body.includes('type=\\"module\\"'));
  assert.ok(body.includes("\\\\o/"));
});

test("checkOutDir: a previous build passes; OPS/, foreign files, sources and nesting don't", () => {
  const ok = { dir: "/w/app/txco-web", protect: ["/w/app", "/w/app/dist", "/w/app/public"], notInside: ["/w/app/dist"], entries: null };
  assert.deepEqual(checkOutDir(ok), []);
  assert.deepEqual(checkOutDir({ ...ok, entries: ["txco-web.json", "public", "ops", ".DS_Store"] }), []);
  assert.deepEqual(checkOutDir({ ...ok, entries: ["nitro.json"], allow: ["nitro.json"] }), []);
  assert.match(checkOutDir({ ...ok, dir: "/w/OPS/web" })[0], /inside OPS\//);
  assert.match(checkOutDir({ ...ok, entries: ["public", "notes.md"] })[0], /"notes.md"/);
  assert.ok(checkOutDir({ ...ok, dir: "/w/app" }).some((p) => /holds \/w\/app\/dist/.test(p)));
  assert.match(checkOutDir({ ...ok, dir: "/w/app/dist/txco-web" }).join("\n"), /is inside \/w\/app\/dist/);
});

test("writeOps replaces ops/; writeManifest validates; listFiles lists", async () => {
  const out = await mkdtemp(join(tmpdir(), "web-abi-producer-"));
  try {
    await mkdir(join(out, "ops/900000"), { recursive: true });
    await writeFile(join(out, "ops/900000/stale.txcl"), "old");
    await writeOps(out, renderOps(cases["builtin-404"]));
    assert.deepEqual(await listFiles(join(out, "ops")), ["900000/page-404.txcl", "900900/not-found.txcl"]);
    await writeManifest(out, { abi: 1, immutable: ["assets/"], "x-producer": { name: "t" } });
    assert.deepEqual(JSON.parse(await readFile(join(out, "txco-web.json"), "utf8")).immutable, ["assets/"]);
    await assert.rejects(writeManifest(out, { abi: 2 }), /txco-web\.json/);
  } finally {
    await rm(out, { recursive: true, force: true });
  }
});

test("looksHashed: Vite's and Nuxt's content hashes, not plain names", () => {
  for (const n of ["index-BxYz12Ab.js", "main-x1XGuNl0.css", "entry.CdEf34Gh.css", "BxYz12Ab.js", "_plugin-vue_export-helper-DlAUqK2U.js"]) assert.ok(looksHashed(n), n);
  for (const n of ["robots.txt", "favicon.svg", "index.html", "my-favorite-icon.svg", "logo.png"]) assert.ok(!looksHashed(n), n);
});

test("publicPrefix: a base path becomes a prefix under public/", () => {
  assert.deepEqual(publicPrefix("/"), { prefix: "", external: false });
  assert.deepEqual(publicPrefix("./"), { prefix: "", external: false });
  assert.deepEqual(publicPrefix(""), { prefix: "", external: false });
  assert.deepEqual(publicPrefix("/app/"), { prefix: "app/", external: false });
  assert.deepEqual(publicPrefix("/docs"), { prefix: "docs/", external: false });
  assert.deepEqual(publicPrefix("https://cdn.example.com/x/"), { prefix: "", external: true });
});

test("copyPublic: copies under the prefix, leaves dot paths out, reports all but .vite/", async () => {
  const root = await mkdtemp(join(tmpdir(), "web-abi-copy-"));
  try {
    const from = join(root, "dist");
    for (const [rel, text] of Object.entries({ "index.html": "home", "assets/a-BxYz12Ab.js": "x", ".vite/manifest.json": "{}", ".well-known/x": "y" })) {
      await mkdir(join(from, rel, ".."), { recursive: true });
      await writeFile(join(from, rel), text);
    }
    const out = join(root, "txco-web");
    const { skipped } = await copyPublic(from, out, "app/");
    assert.deepEqual(skipped, [".well-known"]);
    assert.deepEqual(await listFiles(join(out, "public")), ["app/assets/a-BxYz12Ab.js", "app/index.html"]);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
