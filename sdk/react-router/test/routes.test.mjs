// The route table → matcher translation. The goldens (UPDATE_GOLDEN=1
// rewrites them) are the ops for a sample app; chassis/webabi runs them
// through txcl's RE2 to check what they match.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";

import { renderOps } from "@txco/web-abi/producer";

import { routePaths, routeRegex, routesMatcher } from "../dist/routes.js";

// A sample app: a root layout, an index, a nested layout, params, an
// optional language segment, an optional static segment, a splat, and a
// case-sensitive route.
export const APP = {
  root: { id: "root", path: "" },
  "routes/home": { id: "routes/home", parentId: "root", index: true },
  "routes/about": { id: "routes/about", parentId: "root", path: "about" },
  "routes/users": { id: "routes/users", parentId: "root", path: "users" },
  "routes/users.$id": { id: "routes/users.$id", parentId: "routes/users", path: ":id" },
  "routes/docs": { id: "routes/docs", parentId: "root", path: ":lang?/docs" },
  "routes/beta": { id: "routes/beta", parentId: "root", path: "beta?/pricing" },
  "routes/files": { id: "routes/files", parentId: "root", path: "files/*" },
  "routes/Exact": { id: "routes/Exact", parentId: "root", path: "Exact", caseSensitive: true },
};

test("routePaths joins each route up its parents and dedupes", () => {
  assert.deepEqual(
    routePaths(APP).map((r) => r.path),
    ["", ":lang?/docs", "about", "beta?/pricing", "Exact", "files/*", "users", "users/:id"],
  );
  assert.ok(routePaths(APP).find((r) => r.path === "Exact").caseSensitive);
});

test("routeRegex: React Router's path syntax", () => {
  assert.equal(routeRegex(""), "(?i:/?)");
  assert.equal(routeRegex("about"), "(?i:/about/?)");
  assert.equal(routeRegex("users/:id"), "(?i:/users/[^/]+/?)");
  assert.equal(routeRegex(":lang?/docs"), "(?i:(?:/[^/]+)?/docs/?)");
  assert.equal(routeRegex("beta?/pricing"), "(?i:(?:/beta)?/pricing/?)");
  assert.equal(routeRegex("files/*"), "(?i:/files(?:/.*)?/?)");
  assert.equal(routeRegex("Exact", true), "/Exact/?");
  assert.equal(routeRegex("a.b"), "(?i:/a\\.b/?)");
  assert.equal(routeRegex("file.:ext"), null, "a param inside a segment");
  assert.equal(routeRegex("*/x"), null, "a splat that isn't last");
});

test("routesMatcher: anchored, under basename, / escaped for txcl; null when a route can't translate", () => {
  const m = routesMatcher({ root: { id: "root", path: "" }, a: { id: "a", parentId: "root", path: "about" } }, "/app/");
  assert.equal(m, "^(?:(?i:\\/app)(?i:\\/?)|(?i:\\/app)(?i:\\/about\\/?))$");
  assert.equal(routesMatcher({ x: { id: "x", path: "file.:ext" } }), null);
  assert.equal(routesMatcher({}), null);
});

test("goldens: the ops for the sample app", async () => {
  const golden = new URL("./golden/routes/", import.meta.url).pathname;
  const SHELL = '<!doctype html><html><head><script type="module" src="/assets/entry.client-BxYz12Ab.js"></script></head><body></body></html>';
  const ops = renderOps({
    mode: "routes",
    producer: "@txco/react-router",
    page: { name: "index.html", html: SHELL },
    routes: routesMatcher(APP),
    notes: ["The route set comes from React Router's route table."],
  });
  if (process.env.UPDATE_GOLDEN) {
    await rm(golden, { recursive: true, force: true });
    for (const [rel, text] of Object.entries(ops)) {
      await mkdir(join(golden, rel, ".."), { recursive: true });
      await writeFile(join(golden, rel), text);
    }
    return;
  }
  for (const [rel, text] of Object.entries(ops)) assert.equal(text, await readFile(join(golden, rel), "utf8"), rel);
  const want = [];
  for (const scope of await readdir(golden)) for (const f of await readdir(join(golden, scope))) want.push(`${scope}/${f}`);
  assert.deepEqual(Object.keys(ops).sort(), want.sort());
});
