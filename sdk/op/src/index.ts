// @txco/op — author a txco nano-op.
//
//   import { op } from "@txco/op";
//   export default op(async ({ input, env, log, emit, meta }) => {
//     return { ok: true };
//   });
//
// The handler receives a single OpContext and returns the output envelope
// (sync or async). The runtime (see ./runtime) reads the request on stdin,
// builds the context, awaits the handler, and writes the result to stdout.

/** Per-invocation metadata stamped by the host. */
export interface Meta {
  /** Request id correlating this op with the originating request. */
  rid: string;
  /** Fully-qualified op id, "<stack>/<scope>/<name>". */
  op: string;
  stack: string;
  scope: number;
  name: string;
}

/** Structured logger. Lines go to stderr and surface in the chassis log. */
export interface Logger {
  info(...args: unknown[]): void;
  warn(...args: unknown[]): void;
  error(...args: unknown[]): void;
  debug(...args: unknown[]): void;
}

/** Everything a handler is given. Capability fields (fetch/kv/secrets) are not
 *  part of v1 — they arrive once the host capability + grant model lands. */
export interface OpContext<Input = any, Env = Record<string, unknown>> {
  /** The selected request envelope (the op's input). */
  input: Input;
  /** Trace/op identity for this invocation. */
  meta: Meta;
  /** Non-secret configuration for this op — the resonator's WITH-clause channel. */
  env: Env;
  /** Materialized secrets for this op, by name — the same per-op SecretBag the
   *  chassis splices into an http:// worker's request, here handed to the
   *  in-process compute. Declared in the resonator's WITH `secrets:` block.
   *  Cleartext: returning a secret in the output envelope WILL log/trace it. */
  secrets: Record<string, string>;
  /** Structured logging to the chassis log. */
  log: Logger;
  /** Emit an optional named event (surfaced as a tagged stderr line in v1). */
  emit(event: string, data?: unknown): void;
}

export type OpHandler<Input = any, Output = any> = (
  ctx: OpContext<Input>,
) => Output | Promise<Output>;

/** Brand the handler as a txco op. Today this is the typed authoring surface
 *  and a single hook point; future versions may take options here. */
export function op<Input = any, Output = any>(
  handler: OpHandler<Input, Output>,
): OpHandler<Input, Output> {
  (handler as { __txcoOp?: boolean }).__txcoOp = true;
  return handler;
}
