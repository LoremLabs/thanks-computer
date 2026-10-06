// The validator is held to the corpus the Go validator reads too
// (chassis/webabi), so the two can't drift apart.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { join } from "node:path";

import { publicRoot, validateManifest } from "../dist/manifest.js";

const corpus = new URL("./fixtures/manifests/", import.meta.url).pathname;

test("every valid manifest validates", async () => {
  for (const f of await readdir(join(corpus, "valid"))) {
    const doc = JSON.parse(await readFile(join(corpus, "valid", f), "utf8"));
    const r = validateManifest(doc);
    assert.ok(r.ok, `${f}: ${JSON.stringify(r)}`);
  }
});

test("every invalid manifest fails at the pointer the corpus names", async () => {
  const { invalid } = JSON.parse(await readFile(join(corpus, "cases.json"), "utf8"));
  const files = await readdir(join(corpus, "invalid"));
  assert.equal(files.length, Object.keys(invalid).length);
  for (const [f, pointer] of Object.entries(invalid)) {
    const doc = JSON.parse(await readFile(join(corpus, "invalid", f), "utf8"));
    const r = validateManifest(doc);
    assert.ok(!r.ok, `${f} should be invalid`);
    assert.ok(r.errors.some((e) => e.pointer === pointer), `${f}: want a problem at ${JSON.stringify(pointer)}, got ${JSON.stringify(r.errors)}`);
  }
});

test("publicRoot cuts at the first _ segment", () => {
  for (const [rel, want] of [
    ["index.html", ""],
    ["_app/immutable/x.js", "_app"],
    ["assets/_Dk3.js", "assets/_Dk3.js"],
    ["a/_b/_c/d", "a/_b"],
    ["_root.data", "_root.data"],
  ]) {
    assert.equal(publicRoot(rel), want, rel);
  }
});
