// Internal runtime glue. NOT a public import for op authors — the build step
// injects a call to __run(defaultExport). It reads the request envelope from
// stdin, builds the OpContext, awaits the handler, and writes the output
// envelope to stdout. Diagnostic output (console.*, log, emit) goes to stderr.
//
// ABI v2 stdin shape: { input, meta, env }. stdout is the output envelope only,
// so downstream merge/EMIT/goto semantics are identical to http:// and txco://.

import type { Logger, Meta, OpContext, OpHandler } from "./index";

// Javy globals (enabled via `-J javy-stream-io=y` / `-J text-encoding=y`).
declare const Javy: {
  IO: {
    readSync(fd: number, buf: Uint8Array): number;
    writeSync(fd: number, buf: Uint8Array): number;
  };
};

function readAll(fd: number): Uint8Array {
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const buf = new Uint8Array(4096);
    const n = Javy.IO.readSync(fd, buf);
    if (n === 0) break;
    chunks.push(buf.subarray(0, n));
    total += n;
  }
  const out = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.length;
  }
  return out;
}

function writeAll(fd: number, bytes: Uint8Array): void {
  let off = 0;
  while (off < bytes.length) {
    off += Javy.IO.writeSync(fd, bytes.subarray(off));
  }
}

const enc = new TextEncoder();
const dec = new TextDecoder();

function stderrLine(s: string): void {
  writeAll(2, enc.encode(s + "\n"));
}

function fmt(arg: unknown): string {
  return typeof arg === "string" ? arg : safeStringify(arg);
}

function safeStringify(v: unknown): string {
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

function makeLogger(): Logger {
  const at = (level: string) =>
    (...args: unknown[]) =>
      stderrLine(`[${level}] ` + args.map(fmt).join(" "));
  return { info: at("info"), warn: at("warn"), error: at("error"), debug: at("debug") };
}

export async function __run(handler: OpHandler): Promise<void> {
  // QuickJS's console.* writes to stdout (our JSON channel) — re-point to
  // stderr so a stray console.log can't corrupt the output envelope.
  const log = makeLogger();
  (globalThis as { console?: unknown }).console = {
    log: log.info,
    info: log.info,
    debug: log.debug,
    warn: log.warn,
    error: log.error,
  };

  if (typeof handler !== "function") {
    throw new Error('@txco/op: default export must be op(handler), e.g. export default op(async (ctx) => …)');
  }

  const raw = readAll(0);
  let payload: any = {};
  if (raw.length > 0) {
    payload = JSON.parse(dec.decode(raw));
  }

  // ABI v2 wrapper { input, meta, env }. Tolerate a bare envelope (no wrapper
  // keys) by treating the whole payload as input.
  const wrapped =
    payload != null &&
    typeof payload === "object" &&
    ("input" in payload || "meta" in payload || "env" in payload);

  const meta: Meta = (wrapped && payload.meta) || ({} as Meta);
  const ctx: OpContext = {
    input: wrapped ? payload.input : payload,
    meta,
    env: (wrapped && payload.env) || {},
    secrets: (wrapped && payload.secrets) || {},
    log,
    emit: (event: string, data?: unknown) =>
      stderrLine(safeStringify({ "txco:emit": { event, data } })),
  };

  const out = await handler(ctx);
  writeAll(1, enc.encode(safeStringify(out === undefined ? null : out)));
}
