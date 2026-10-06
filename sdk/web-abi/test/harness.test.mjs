import { test } from "node:test";
import assert from "node:assert/strict";

import { checkServer, loadServer, serve } from "../dist/harness.js";

const app = (name) => new URL(`./fixtures/apps/${name}/`, import.meta.url).pathname;

test("checkServer passes a handler that honours the contract", async () => {
  const results = await checkServer(app("hello"));
  assert.ok(results.every((r) => r.ok), JSON.stringify(results));
});

test("checkServer fails a handler that throws, and one with no default export", async () => {
  const thrown = await checkServer(app("throws"));
  assert.ok(thrown.some((r) => !r.ok && /threw/.test(r.detail)), JSON.stringify(thrown));
  await assert.rejects(loadServer(app("no-default")), /default export/);
  const none = await checkServer(app("no-default"));
  assert.equal(none[0].ok, false);
});

test("serve: public/ first, then the server, both cookies intact", async () => {
  const server = await serve(app("hello"));
  try {
    const base = `http://127.0.0.1:${server.address().port}`;
    assert.equal(await (await fetch(`${base}/robots.txt`)).text(), "hello file\n");
    const home = await fetch(`${base}/`);
    assert.equal(await home.text(), "home");
    assert.deepEqual(home.headers.getSetCookie(), ["a=1; Path=/", "b=2; Path=/"]);
    const post = await fetch(`${base}/x`, { method: "POST", body: "{}" });
    assert.equal(post.status, 201);
    assert.match(await post.text(), /^posted \{\} from /);
    assert.equal((await fetch(`${base}/nope`)).status, 404);
  } finally {
    server.close();
  }
});
