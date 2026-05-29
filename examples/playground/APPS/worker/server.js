// A tiny async-continuation worker. `txco dev` launches it (declared as
// the `worker` app in txco.yaml) and the `hello-world/150` rule EXECs it
// with `WITH mode = "async"`.
//
// The async contract:
//   1. The chassis POSTs { "input": <op input>, "_txc": { … } } and the
//      single-use callback credential in the X-Txco-Continuation-Token
//      request header.
//   2. The worker MUST answer 202 Accepted promptly (optionally a
//      {"job_id":"…"} body) — it does NOT return the result inline.
//   3. Later (here: after a simulated 5s job) the worker POSTs the real
//      result back to _txc.callback_url with
//      `Authorization: Bearer <token>` and a body of either
//      {"status":"complete","output":{…}} or
//      {"status":"failed","error":{…}}.
//
// No npm dependencies — only Node's built-in http/URL.

const http = require("node:http");

const PORT = 9009;

// Flip to true to exercise the failure path (run derives `failed`,
// no result.json).
const SIMULATE_FAILURE = false;

// How long the "long-running job" takes before calling back. The
// default (~20-26s) simulates a realistically slow job so a human
// running the example sees the chassis promote to a continuation and
// land on the wait page. Override with WORKER_JOB_MS (milliseconds) —
// the examples-smoke harness sets a small value so the 202→poll→200
// continuation path is exercised end-to-end without the long wait.
const JOB_MS = Number(process.env.WORKER_JOB_MS) || (20000 + Math.floor(Math.random() * 6000));

function postCallback(callbackURL, token, body) {
  const u = new URL(callbackURL);
  const payload = JSON.stringify(body);
  const req = http.request(
    {
      hostname: u.hostname,
      port: u.port || 80,
      path: u.pathname + u.search,
      method: "POST",
      headers: {
        "content-type": "application/json",
        authorization: "Bearer " + token,
        "content-length": Buffer.byteLength(payload),
      },
    },
    (res) => {
      let rb = "";
      res.on("data", (c) => (rb += c));
      res.on("end", () =>
        console.log(`[worker] callback -> ${res.statusCode} ${rb.trim()}`),
      );
    },
  );
  req.on("error", (e) => console.error("[worker] callback error:", e.message));
  req.end(payload);
}

const server = http.createServer((req, res) => {
  if (req.method === "GET" && req.url === "/health") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(`{"ok":true}`);
    return;
  }

  if (req.method === "POST" && req.url === "/research") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      let env = {};
      try {
        env = JSON.parse(body || "{}");
      } catch (_) {}
      const txc = env._txc || {};
      const token = req.headers["x-txco-continuation-token"] || "";

      console.log(
        `[worker] accepted op_continuation=${txc.op_continuation_id} ` +
          `run_continuation=${txc.run_continuation_id} stage=${txc.stage} ` +
          `callback=${txc.callback_url}`,
      );

      // Step 2: ack immediately. The chassis suspends the run and the
      // client gets a 202 + Location.
      res.writeHead(202, { "content-type": "application/json" });
      res.end(JSON.stringify({ status: "accepted", job_id: "job-demo-1" }));

      // Step 3: simulate a slow job, then post the result back.
      setTimeout(() => {
        if (SIMULATE_FAILURE) {
          postCallback(txc.callback_url, token, {
            status: "failed",
            error: { code: "DEMO_FAIL", message: "simulated worker failure" },
          });
          return;
        }
        postCallback(txc.callback_url, token, {
          status: "complete",
          output: {
            research: {
              summary: "async research complete",
              sources: ["https://example.com/a", "https://example.com/b"],
            },
          },
        });
      }, JOB_MS);
    });
    return;
  }

  res.writeHead(404, { "content-type": "application/json" });
  res.end(`{"error":"not found"}`);
});

server.listen(PORT, () => console.log(`[worker] listening on :${PORT}`));
