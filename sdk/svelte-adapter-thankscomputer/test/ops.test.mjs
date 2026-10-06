// Golden tests for the ops the adapter writes. UPDATE_GOLDEN=1 rewrites them;
// review the diff. chassis/webabi strict-parses the same goldens with txcl.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile, writeFile, mkdir, readdir, rm } from "node:fs/promises";
import { join } from "node:path";

import { catchAllScope, knownRoutesRegex, renderOps } from "../dist/ops.js";

const golden = new URL("./golden/", import.meta.url).pathname;
const SHELL = '<!doctype html><html><head><script type="module">import("/app/immutable/start.js")</script></head><body>\\o/</body></html>';
const page = (re) => ({ pattern: re, page: { methods: ["GET"] } });
const opts = { scope: 900000, shell: SHELL, fallbackName: "200.html", producer: "@txco/svelte-adapter-thankscomputer" };

const cases = {
  "route-aware": [page(/^\/$/), page(/^\/login\/?$/), page(/^\/book\/([^/]+?)\/?$/), { pattern: /^\/api\/x\/?$/, page: null }],
  "lookaround": [page(/^\/$/), page(/^\/(?!admin)([^/]+?)\/?$/)],
  "no-page-routes": [{ pattern: /^\/api\/x\/?$/, page: null }],
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

for (const [name, routes] of Object.entries(cases)) {
  test(`renderOps: ${name}`, async () => check(name, renderOps(routes, opts)));
}

test("renderOps: no fallback shell writes only the catch-all", async () => {
  await check("no-fallback", renderOps(cases["route-aware"], { ...opts, shell: null }));
});

test("the catch-all sits 900 above the navigation ops", () => {
  assert.equal(catchAllScope(900000), 900900);
  assert.ok(Object.keys(renderOps([], opts)).includes("900900/not-found.txcl"));
});

test("knownRoutesRegex: page routes only, anchored, slashes escaped; null for lookaround", () => {
  assert.equal(knownRoutesRegex(cases["route-aware"]), "^(?:(?:\\/)|(?:\\/login\\/?)|(?:\\/book\\/([^\\/]+?)\\/?))$");
  assert.equal(knownRoutesRegex(cases["lookaround"]), null);
  assert.equal(knownRoutesRegex(cases["no-page-routes"]), null);
});

test("the shell's quotes and backslashes are escaped in the b64 literal", () => {
  const ops = renderOps(cases["route-aware"], opts);
  const body = ops["900000/spa-fallback.txcl"];
  assert.ok(body.includes('type=\\"module\\"'), "quotes escaped");
  assert.ok(body.includes("\\\\o/"), "backslash escaped");
});
