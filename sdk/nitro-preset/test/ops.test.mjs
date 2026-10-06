// Golden tests for the ops the preset writes. UPDATE_GOLDEN=1 rewrites them;
// review the diff. chassis/webabi strict-parses the same goldens with txcl.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile, writeFile, mkdir, readdir, rm } from "node:fs/promises";
import { join } from "node:path";

import { BUILTIN_404, catchAllScope, renderOps } from "../dist/ops.js";

const golden = new URL("./golden/", import.meta.url).pathname;
const SHELL = '<!doctype html><html><head><script type="module" src="/_nuxt/entry.BxYz12Ab.js"></script></head><body><div id="__nuxt">\\o/</div></body></html>';
const base = { scope: 900000, producer: "@txco/nitro-preset" };

const cases = {
  "static-404": { ...base, mode: "static", page: { name: "404.html", html: SHELL } },
  "static-builtin-404": { ...base, mode: "static", page: null },
  "spa": { ...base, mode: "spa", page: { name: "200.html", html: SHELL } },
  "server": { ...base, mode: "server", page: null },
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

test("an spa build without its shell is refused", () => {
  assert.throws(() => renderOps({ ...base, mode: "spa", page: null }), /needs its shell/);
});

test("the catch-all sits 900 above the navigation op, and follows the scope option", () => {
  assert.equal(catchAllScope(900000), 900900);
  assert.deepEqual(Object.keys(renderOps({ ...base, scope: 950000, mode: "static", page: null })).sort(), ["950000/page-404.txcl", "950900/not-found.txcl"]);
});

test("the page's quotes and backslashes are escaped in the b64 literal", () => {
  const body = renderOps(cases["static-404"])["900000/page-404.txcl"];
  assert.ok(body.includes('type=\\"module\\"'), "quotes escaped");
  assert.ok(body.includes("\\\\o/"), "backslash escaped");
  assert.ok(renderOps(cases["static-builtin-404"])["900000/page-404.txcl"].includes("404 Not Found"));
  assert.ok(BUILTIN_404.startsWith("<!doctype html>"));
});
