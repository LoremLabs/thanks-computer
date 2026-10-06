import { test } from "node:test";
import assert from "node:assert/strict";

import { checkOutDir } from "../dist/guard.js";

const ok = {
  dir: "/w/app/txco-web",
  publicDir: "/w/app/txco-web/public",
  serverDir: "/w/app/txco-web/server",
  baseURL: "/",
  protect: ["/w/app", "/w/app/app", "/w/app/.nuxt", "/w/app/public"],
  entries: null,
};

test("the preset's own layout passes, fresh or over a previous build", () => {
  assert.deepEqual(checkOutDir(ok), []);
  assert.deepEqual(checkOutDir({ ...ok, entries: ["txco-web.json", "public", "ops", "server", "nitro.json", ".DS_Store"] }), []);
  assert.deepEqual(checkOutDir({ ...ok, baseURL: "/docs/", publicDir: "/w/app/txco-web/public/docs" }), []);
});

test("an output dir inside OPS/ is refused", () => {
  const p = checkOutDir({ ...ok, dir: "/w/OPS/web", publicDir: "/w/OPS/web/public", serverDir: "/w/OPS/web/server" });
  assert.equal(p.length, 1);
  assert.match(p[0], /inside OPS\//);
});

test("foreign entries are refused: the build wipes the dir", () => {
  const p = checkOutDir({ ...ok, entries: ["public", "notes.md", "FILES"] });
  assert.equal(p.length, 1);
  assert.match(p[0], /"notes.md", "FILES"/);
});

test("a dir holding the app's sources is refused", () => {
  const p = checkOutDir({ ...ok, dir: "/w/app", publicDir: "/w/app/public", serverDir: "/w/app/server" });
  assert.ok(p.some((m) => /holds \/w\/app\b/.test(m)), p.join("\n"));
});

test("a framework's own output paths (Analog with only dir overridden) are refused with the fix", () => {
  const p = checkOutDir({ ...ok, publicDir: "/w/app/dist/analog/public" });
  assert.equal(p.length, 1);
  assert.match(p[0], /set nitro\.output\.dir and nitro\.output\.publicDir together/);
});
