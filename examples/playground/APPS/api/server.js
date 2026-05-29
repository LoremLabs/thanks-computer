// A tiny single-process HTTP server exposing four op-handler routes plus
// a health check. `txco dev` launches this once and points four separate
// operations (op://HELLO, op://WORLD, op://SORT, op://RENDER) at four
// routes on this same Node process.
//
// No npm dependencies — only Node's built-in `http` module. Run directly:
//
//   node server.js
//
// The chassis POSTs the current envelope as the request body for every
// op dispatch. Each handler reads the envelope, returns JSON, and the
// chassis deep-merges that JSON back into the envelope before the next
// stage runs.

const http = require("node:http");

const PORT = 4100;

const routes = {
  // Stage 100: two parallel ops, each contributing one word.
  // The chassis appends arrays during the deep-merge, so the envelope
  // arrives at stage 200 with `words: ['hello', 'world']` (or 'world',
  // 'hello' — parallel order isn't deterministic).
  "/words/hello": async () => {
    await new Promise((r) => setTimeout(r, 700));
    return { words: ["hello"] };
  },
  "/words/world": async () => {
    // Artificial 1s delay so parallel-vs-serial scheduling is visible
    // in the admin-UI trace: with parallel scope-100 execution, the
    // stack wall-clock stays ~1s; if it climbs toward 2s the
    // scheduler is running siblings serially.
    await new Promise((r) => setTimeout(r, 800));
    return { words: ["world"] };
  },

  // Stage 200: sort the merged words alphabetically. Stable result
  // regardless of which parallel op finished first at stage 100.
  "/sort": (envelope) => {
    const words = Array.isArray(envelope.words) ? [...envelope.words] : [];

    // ideal order: hello cruel world, so special sort function
    // that puts "hello" first, "world" last, and "cruel" in the middle.

    let sorted_words = words.sort((a, b) => {
      if (a === "hello") return -1;
      if (b === "hello") return 1;
      if (a === "world") return 1;
      if (b === "world") return -1;
      return a.localeCompare(b);
    });

    return { sorted_words };
  },

  // Stage 1000: render the sorted words as HTML and set the chassis's
  // response shape directly. `_txc.web.res.body` is base64-encoded;
  // `_txc.web.res.headers.content-type` becomes the HTTP response
  // Content-Type. The web inlet returns the decoded body verbatim,
  // dropping the JSON envelope.
  "/render": (envelope) => {
    const text = Array.isArray(envelope.sorted_words)
      ? envelope.sorted_words.join(" ")
      : "";
    const html = `<!doctype html>
<html>
  <head><meta charset="utf-8"><title>${text}</title></head>
  <body><h1>${text}</h1></body>
</html>
`;
    return {
      _txc: {
        web: {
          res: {
            headers: { "content-type": ["text/html; charset=utf-8"] },
            body: Buffer.from(html, "utf-8").toString("base64"),
          },
        },
      },
    };
  },
};

const server = http.createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", async () => {
    if (req.url === "/health") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      res.end("ok\n");
      return;
    }
    const handler = routes[req.url];
    if (!handler) {
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found\n");
      return;
    }

    let envelope = {};
    const raw = Buffer.concat(chunks).toString();
    if (raw) {
      try {
        envelope = JSON.parse(raw);
      } catch (err) {
        res.writeHead(400, { "Content-Type": "text/plain" });
        res.end("invalid JSON\n");
        return;
      }
    }

    // The chassis stamps every outbound envelope with the firing rule's
    // identity. Echo it so the dev pane shows what's running where.
    // _txc.tenant is only present when an ingress.yaml routed the event;
    // skip it on the unrouted fallback path so the log stays compact.
    const op = envelope?._txc?.op ?? "(missing)";
    const step = envelope?._txc?.step ?? "(missing)";
    const tenant = envelope?._txc?.tenant;
    const t = tenant ? `  _txc.tenant=${tenant}` : "";
    console.log(`${req.url}  _txc.op=${op}  _txc.step=${step}${t}`);

    const result = await handler(envelope);
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(result));
  });
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`api listening on :${PORT}`);
});
