import { test } from "node:test";
import assert from "node:assert/strict";

import { checkImmutable, deriveImmutable, looksHashed, YEAR } from "../dist/immutable.js";

// What Nuxt 4.6 registers (Nitro normalizes baseURLs to "/x").
const nuxt = [
  { baseURL: "/", maxAge: 0 },
  { baseURL: "/_nuxt", maxAge: YEAR },
  { baseURL: "/_nuxt/builds/meta", maxAge: YEAR },
  { baseURL: "/_nuxt/builds", maxAge: 1 },
];

test("Nuxt: _nuxt/, its cache-busted builds/ dir notwithstanding", () => {
  assert.deepEqual(deriveImmutable({ assets: nuxt, appBaseURL: "/", framework: "nuxt" }), ["_nuxt/"]);
});

test("the app's baseURL prefixes the derived prefix", () => {
  assert.deepEqual(deriveImmutable({ assets: nuxt, appBaseURL: "/docs/", framework: "nuxt" }), ["docs/_nuxt/"]);
});

test("a short-lived dir inside a candidate drops it, outside Nuxt's builds/ exception", () => {
  assert.deepEqual(deriveImmutable({ assets: nuxt, appBaseURL: "/", framework: "nitro" }), ["_nuxt/builds/meta/"]);
  assert.deepEqual(
    deriveImmutable({ assets: [{ baseURL: "/assets", maxAge: YEAR }, { baseURL: "/assets", maxAge: 60 }], appBaseURL: "/", framework: "nitro" }),
    [],
  );
});

test("nothing is derived without a year's maxAge (Analog)", () => {
  assert.deepEqual(deriveImmutable({ assets: [{ dir: "x" }], appBaseURL: "/", framework: "analog" }), []);
  assert.deepEqual(deriveImmutable({ assets: [{ baseURL: "/", maxAge: YEAR }], appBaseURL: "/", framework: "nitro" }), []);
});

test("nested candidates collapse into the outer one", () => {
  assert.deepEqual(
    deriveImmutable({ assets: [{ baseURL: "/a", maxAge: YEAR }, { baseURL: "/a/b", maxAge: YEAR }, { baseURL: "/c/", maxAge: YEAR }], appBaseURL: "/", framework: "nitro" }),
    ["a/", "c/"],
  );
});

test("checkImmutable: directories relative to public/, ending in /", () => {
  assert.deepEqual(checkImmutable(["assets/", "a/b/"]), []);
  for (const bad of ["assets", "/assets/", "/", "a//b/", "./x/", "_txco/x/"]) {
    assert.equal(checkImmutable([bad]).length, 1, bad);
  }
});

test("looksHashed: Vite's content hashes, not plain names", () => {
  for (const n of ["BxYz12Ab.js", "entry.CdEf34Gh.css", "0.DKlG8jTr.css", "a97LY-W2.js", "f3b2c1d0-9a8b-4c7d-8e6f-5a4b3c2d1e0f.json"]) assert.ok(looksHashed(n), n);
  for (const n of ["robots.txt", "favicon.ico", "latest.json", "index.html", "abcdefgh.js"]) assert.ok(!looksHashed(n), n);
});
