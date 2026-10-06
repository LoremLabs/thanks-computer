// The harness: load a build's server/ entry and check it honours the
// contract, before any runner exists; and serve a build locally behind the
// same bridge a runner will use.

import { createServer, type IncomingMessage, type Server } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { extname, join, normalize, sep } from "node:path";
import { pathToFileURL } from "node:url";

import { dispatch } from "./bridge.js";
import type { FetchHandler, TxcEnvelope } from "./envelope.js";
import { readManifest } from "./manifest.js";

/** Imports a build's server entry and checks its default export. */
export async function loadServer(abiDir: string): Promise<FetchHandler> {
  const m = await readManifest(abiDir);
  if (!m.server) throw new Error("the manifest names no server.entry: this is a static build");
  const mod = await import(pathToFileURL(join(abiDir, m.server.entry)).href);
  const h = mod.default as FetchHandler | undefined;
  if (!h || typeof h.fetch !== "function") {
    throw new Error(`${m.server.entry}: the default export must be { fetch(request, ctx) }`);
  }
  return h;
}

/** Builds an envelope the way the chassis's web head does, for a request. */
export function envelopeFor(method: string, path: string, init: { headers?: Record<string, string[]>; body?: Uint8Array; host?: string; ip?: string } = {}): TxcEnvelope {
  const host = init.host ?? "localhost";
  const q = path.indexOf("?");
  return {
    _txc: {
      client: { ip: init.ip ?? "127.0.0.1" },
      web: {
        req: {
          method,
          host,
          url: { full: `http://${host}${path}`, path: q < 0 ? path : path.slice(0, q), query: { raw: q < 0 ? "" : path.slice(q + 1) } },
          headers: init.headers ?? {},
          body: init.body && init.body.byteLength > 0 ? Buffer.from(init.body).toString("base64") : undefined,
        },
      },
    },
  };
}

export interface CheckResult {
  name: string;
  ok: boolean;
  detail?: string;
}

/**
 * Drives a server entry with the requests every handler must answer, through
 * the bridge, and checks each answer is a valid delta: a status in
 * 100–599, headers as arrays, no echo of the request envelope, a body only
 * where one is allowed.
 */
export async function checkServer(abiDir: string): Promise<CheckResult[]> {
  const results: CheckResult[] = [];
  let handler: FetchHandler;
  try {
    handler = await loadServer(abiDir);
    results.push({ name: "load", ok: true });
  } catch (e) {
    return [{ name: "load", ok: false, detail: (e as Error).message }];
  }
  const probes: Array<[string, TxcEnvelope, string]> = [
    ["GET /", envelopeFor("GET", "/", { headers: { accept: ["text/html"] } }), "GET"],
    ["GET unknown page", envelopeFor("GET", "/txco-web-abi-check-unknown", { headers: { accept: ["text/html"] } }), "GET"],
    ["POST unknown", envelopeFor("POST", "/txco-web-abi-check-unknown", { headers: { "content-type": ["application/json"] }, body: new TextEncoder().encode("{}") }), "POST"],
    ["HEAD /", envelopeFor("HEAD", "/"), "HEAD"],
  ];
  for (const [name, env, method] of probes) {
    const delta = await dispatch(handler, env);
    const res = delta?._txc?.web?.res;
    const problems: string[] = [];
    if (!res || !Number.isInteger(res.status) || res.status < 100 || res.status > 599) problems.push("no valid status");
    if (res && Object.values(res.headers).some((v) => !Array.isArray(v))) problems.push("headers must be arrays");
    if ((delta as unknown as { _txc: { web: { req?: unknown } } })._txc.web.req !== undefined) problems.push("the answer echoes the request");
    if (method === "HEAD" && res?.body) problems.push("a HEAD answer carries a body");
    if (res?.status === 500 && (method === "HEAD" || (res.body && Buffer.from(res.body, "base64").toString() === "internal error\n"))) {
      problems.push("the handler threw or returned no Response (a 500 from the bridge)");
    }
    try {
      JSON.stringify(delta);
    } catch {
      problems.push("the delta isn't serialisable");
    }
    results.push({ name, ok: problems.length === 0, detail: problems.join("; ") || `→ ${res?.status}` });
  }
  return results;
}

const TYPES: Record<string, string> = {
  ".html": "text/html; charset=utf-8", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
  ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg",
  ".webp": "image/webp", ".ico": "image/x-icon", ".txt": "text/plain; charset=utf-8", ".woff2": "font/woff2",
};

async function readBody(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = [];
  for await (const c of req) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/**
 * Serves a build locally: public/ files first (as the chassis's static
 * serving answers first), everything else through the bridge to server/.
 * For local development; not a chassis.
 */
export async function serve(abiDir: string, opts: { port?: number; host?: string } = {}): Promise<Server> {
  const handler = await loadServer(abiDir);
  const pub = join(abiDir, "public");
  const server = createServer(async (req, res) => {
    const method = (req.method ?? "GET").toUpperCase();
    const path = new URL(req.url ?? "/", "http://localhost").pathname;
    if (method === "GET" || method === "HEAD") {
      const segs = path.split("/").filter(Boolean);
      if (!segs.some((s) => s.startsWith("."))) {
        const file = normalize(join(pub, ...segs, path.endsWith("/") ? "index.html" : ""));
        if (file.startsWith(pub + sep) || file === pub) {
          try {
            if ((await stat(file)).isFile()) {
              const bytes = await readFile(file);
              res.writeHead(200, { "content-type": TYPES[extname(file)] ?? "application/octet-stream" });
              res.end(method === "HEAD" ? undefined : bytes);
              return;
            }
          } catch {
            // not a file: the server answers
          }
        }
      }
    }
    const headers: Record<string, string[]> = {};
    for (let i = 0; i < req.rawHeaders.length; i += 2) {
      (headers[req.rawHeaders[i].toLowerCase()] ??= []).push(req.rawHeaders[i + 1]);
    }
    const delta = await dispatch(handler, envelopeFor(method, req.url ?? "/", {
      headers, body: await readBody(req), host: req.headers.host, ip: req.socket.remoteAddress ?? "",
    }));
    const out = delta._txc.web.res;
    for (const [name, values] of Object.entries(out.headers)) res.setHeader(name, values);
    res.writeHead(out.status);
    res.end(out.body ? Buffer.from(out.body, "base64") : undefined);
  });
  await new Promise<void>((resolve) => server.listen(opts.port ?? 0, opts.host ?? "127.0.0.1", resolve));
  return server;
}
