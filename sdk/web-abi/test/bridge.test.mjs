import { test } from "node:test";
import assert from "node:assert/strict";

import { contextFrom, dispatch, envelopeToRequest, responseToDelta } from "../dist/bridge.js";
import { envelopeFor } from "../dist/harness.js";

test("envelopeToRequest: method, url, every header value, the body", async () => {
  const env = envelopeFor("POST", "/a/b?x=1", {
    headers: { accept: ["text/html", "application/json"], "content-length": ["2"], host: ["evil"] },
    body: new TextEncoder().encode("{}"),
  });
  const req = envelopeToRequest(env);
  assert.equal(req.method, "POST");
  assert.equal(req.url, "http://localhost/a/b?x=1");
  assert.equal(req.headers.get("accept"), "text/html, application/json");
  assert.equal(req.headers.get("host"), null, "hop-by-hop and computed headers are dropped");
  assert.equal(await req.text(), "{}");
});

test("envelopeToRequest: GET and HEAD carry no body; a URL is built without url.full", () => {
  const env = { _txc: { web: { req: { method: "GET", host: "ex.com", url: { path: "/p", query: { raw: "q=2" } }, body: "e30=" } } } };
  const req = envelopeToRequest(env);
  assert.equal(req.url, "http://ex.com/p?q=2");
  assert.equal(req.body, null);
});

test("envelopeToRequest refuses a body over the limit", () => {
  const env = envelopeFor("POST", "/", { body: new Uint8Array(10) });
  assert.throws(() => envelopeToRequest(env, { maxBodyBytes: 5 }));
});

test("contextFrom is minimal and frozen", () => {
  const ctx = contextFrom(envelopeFor("GET", "/", { ip: "203.0.113.7" }));
  assert.deepEqual(ctx, { client: { ip: "203.0.113.7" } });
  assert.ok(Object.isFrozen(ctx) && Object.isFrozen(ctx.client));
});

test("responseToDelta: every Set-Cookie on its own line, a base64 body, halt", async () => {
  const h = new Headers({ "content-type": "text/plain" });
  h.append("set-cookie", "a=1");
  h.append("set-cookie", "b=2");
  const delta = await responseToDelta(new Response("hi", { status: 201, headers: h }));
  assert.deepEqual(delta._txc.web.res.headers["set-cookie"], ["a=1", "b=2"]);
  assert.deepEqual(delta._txc.web.res.headers["content-type"], ["text/plain"]);
  assert.equal(Buffer.from(delta._txc.web.res.body, "base64").toString(), "hi");
  assert.equal(delta._txc.halt, true);
  assert.equal(delta._txc.web.req, undefined, "never an echo of the request");
});

test("responseToDelta: no body for HEAD, 204 and 304", async () => {
  for (const [res, method] of [[new Response("x"), "HEAD"], [new Response(null, { status: 204 }), "GET"], [new Response(null, { status: 304 }), "GET"]]) {
    const d = await responseToDelta(res, { method });
    assert.equal(d._txc.web.res.body, undefined);
  }
});

test("responseToDelta refuses a body over the answer limit", async () => {
  await assert.rejects(responseToDelta(new Response("x".repeat(10)), { maxAnswerBytes: 5 }));
});

test("dispatch: a throw, or no Response, is a 500 that leaks nothing", async () => {
  for (const handler of [{ fetch() { throw new Error("secret detail"); } }, { fetch() { return "not a response"; } }]) {
    const d = await dispatch(handler, envelopeFor("GET", "/"));
    assert.equal(d._txc.web.res.status, 500);
    assert.ok(!Buffer.from(d._txc.web.res.body, "base64").toString().includes("secret"));
  }
});
