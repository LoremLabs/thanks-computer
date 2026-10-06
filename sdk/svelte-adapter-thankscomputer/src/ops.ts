// The ops this adapter writes into a Web ABI build's ops/. Pure: no
// filesystem and no SvelteKit runtime, so the output is golden-tested. The
// rules themselves come from @txco/web-abi/producer, which every producer
// shares; this file turns SvelteKit's route table into a matcher.
import { catchAllScope, renderOps as renderABIOps } from "@txco/web-abi/producer";

export { catchAllScope };

/** The part of a SvelteKit route definition the matcher reads. */
export interface RouteLike {
  pattern: RegExp;
  page?: { methods: readonly string[] } | null;
}

export interface RenderOptions {
  /** The producer band's scope for the navigation ops (900000). */
  scope: number;
  /** The fallback shell's HTML, or null when the app has no SPA fallback. */
  shell: string | null;
  /** The shell's file name in public/, for the comments. */
  fallbackName: string;
  /** The generated-file header's producer name. */
  producer: string;
}

/**
 * Renders the build's ops, as path (relative to ops/) → file text:
 *
 *  - with a route table RE2 can express: spa-fallback (200 + the shell) for a
 *    known page route, spa-404 (404 + the shell) for any other navigation;
 *  - otherwise, or with no page routes: a single spa-fallback, 200 for every
 *    navigation;
 *  - always: not-found, a plain 404 for every other HTTP request, so every
 *    request reaching the end of the stack is answered.
 *
 * With no shell (fallback: false) only the catch-all is written.
 */
export function renderOps(routes: readonly RouteLike[], opts: RenderOptions): Record<string, string> {
  const base = { scope: opts.scope, producer: opts.producer };
  if (opts.shell === null) return renderABIOps({ ...base, mode: "none", page: null });
  const page = { name: opts.fallbackName, html: opts.shell };
  const known = knownRoutesRegex(routes);
  if (known === null) {
    return renderABIOps({
      ...base,
      mode: "spa",
      page,
      notes: ["SvelteKit's route table couldn't be expressed as a matcher (or has no page routes)."],
    });
  }
  return renderABIOps({ ...base, mode: "routes", page, routes: known, notes: ["The route set comes from SvelteKit's own route table."] });
}

/**
 * An anchored regex STRING matching exactly the SvelteKit page routes, from
 * the framework's route table. null when there are no page routes, or when
 * a route needs a regex feature Go's RE2 (which txcl compiles with) lacks.
 * Literal slashes are escaped for the txcl /.../ delimiter.
 */
export function knownRoutesRegex(routes: readonly RouteLike[]): string | null {
  const parts: string[] = [];
  for (const r of routes) {
    if (!r.page || r.page.methods.length === 0) continue; // +server.ts / api-only
    let src = r.pattern.source;
    if (/\(\?[=!<]/.test(src) || /\\[1-9]/.test(src)) return null;
    src = src.replace(/^\^/, "").replace(/\$$/, "");
    src = src.replace(/\\?\//g, "\\/");
    parts.push(`(?:${src})`);
  }
  return parts.length ? `^(?:${parts.join("|")})$` : null;
}
