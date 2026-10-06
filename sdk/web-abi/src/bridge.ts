// The envelope ↔ Fetch bridge. A runner hands it the chassis's request
// envelope; it builds a Fetch Request, calls the server entry, and turns the
// Response into the delta the chassis merges. Every runner uses this one
// mapping, so a handler sees the same Request wherever it runs.

import type { FetchHandler, TxcContext, TxcDelta, TxcEnvelope } from "./envelope.js";

/** An op answer is capped at 4 MiB (--op-payload-max); the body is base64 inside it. */
export const DEFAULT_MAX_ANSWER_BYTES = 3 * 1024 * 1024;

// Hop-by-hop headers (RFC 9110 §7.6.1) and those the Request computes.
const DROPPED_HEADERS = new Set([
  "connection", "keep-alive", "proxy-connection", "transfer-encoding", "te", "trailer",
  "upgrade", "content-length", "host",
]);

export interface RequestOptions {
  /** Refuse a request body larger than this (bytes). Default: no limit. */
  maxBodyBytes?: number;
}

/** Builds the Fetch Request an envelope describes. */
export function envelopeToRequest(env: TxcEnvelope, opts: RequestOptions = {}): Request {
  const req = env._txc?.web?.req ?? {};
  const method = (req.method ?? "GET").toUpperCase();
  let url = req.url?.full;
  if (!url) {
    const host = req.host ?? "localhost";
    const raw = req.url?.query?.raw;
    url = `http://${host}${req.url?.path ?? "/"}${raw ? "?" + raw : ""}`;
  }
  const headers = new Headers();
  for (const [name, value] of Object.entries(req.headers ?? {})) {
    if (DROPPED_HEADERS.has(name.toLowerCase())) continue;
    for (const v of Array.isArray(value) ? value : [value]) headers.append(name, v);
  }
  let body: ArrayBuffer | undefined;
  if (req.body && method !== "GET" && method !== "HEAD") {
    const bytes = Buffer.from(req.body, "base64");
    if (opts.maxBodyBytes !== undefined && bytes.byteLength > opts.maxBodyBytes) {
      throw new Error(`request body is ${bytes.byteLength} bytes, over ${opts.maxBodyBytes}`);
    }
    body = new ArrayBuffer(bytes.byteLength);
    new Uint8Array(body).set(bytes);
  }
  return new Request(url, { method, headers, body });
}

/** The handler's context: only what the contract defines. */
export function contextFrom(env: TxcEnvelope): TxcContext {
  return Object.freeze({ client: Object.freeze({ ip: env._txc?.client?.ip ?? "" }) });
}

export interface DeltaOptions {
  /** The request's method: a HEAD answer carries no body. */
  method?: string;
  /** Refuse a body larger than this (bytes). */
  maxAnswerBytes?: number;
}

/** Turns a Response into the chassis delta. */
export async function responseToDelta(res: Response, opts: DeltaOptions = {}): Promise<TxcDelta> {
  const headers: Record<string, string[]> = {};
  res.headers.forEach((value, name) => {
    if (name === "set-cookie") return; // each cookie on its own line, below
    (headers[name] ??= []).push(value);
  });
  const cookies = res.headers.getSetCookie();
  if (cookies.length > 0) headers["set-cookie"] = cookies;

  const delta: TxcDelta = { _txc: { web: { res: { status: res.status, headers } }, halt: true } };
  const noBody = (opts.method ?? "GET").toUpperCase() === "HEAD" || res.status === 204 || res.status === 304;
  if (!noBody) {
    const bytes = Buffer.from(await res.arrayBuffer());
    const max = opts.maxAnswerBytes ?? DEFAULT_MAX_ANSWER_BYTES;
    if (bytes.byteLength > max) {
      throw new Error(`response body is ${bytes.byteLength} bytes, over the ${max}-byte answer limit`);
    }
    if (bytes.byteLength > 0) delta._txc.web.res.body = bytes.toString("base64");
  }
  return delta;
}

function errorDelta(status: number, message: string, method: string): TxcDelta {
  const res: TxcDelta["_txc"]["web"]["res"] = { status, headers: { "content-type": ["text/plain; charset=utf-8"] } };
  if (method.toUpperCase() !== "HEAD") res.body = Buffer.from(message + "\n").toString("base64");
  return { _txc: { web: { res }, halt: true } };
}

/**
 * Runs one request through a handler, end to end. A handler that throws, or
 * returns something other than a Response, answers 500; the error is never
 * shown to the client.
 */
export async function dispatch(handler: FetchHandler, env: TxcEnvelope, opts: RequestOptions & DeltaOptions = {}): Promise<TxcDelta> {
  const method = env._txc?.web?.req?.method ?? "GET";
  let request: Request;
  try {
    request = envelopeToRequest(env, opts);
  } catch {
    return errorDelta(413, "request body too large", method);
  }
  let res: unknown;
  try {
    res = await handler.fetch(request, contextFrom(env));
  } catch {
    return errorDelta(500, "internal error", method);
  }
  if (!(res instanceof Response)) return errorDelta(500, "internal error", method);
  try {
    return await responseToDelta(res, { ...opts, method: request.method });
  } catch {
    return errorDelta(502, "response too large", method);
  }
}
