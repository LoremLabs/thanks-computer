// The ops this adapter writes into a Web ABI build's ops/. Pure: no
// filesystem and no SvelteKit runtime, so the output is golden-tested.

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

/** Where the catch-all sits: after the navigation ops, which run concurrently in one scope. */
export const catchAllScope = (scope: number): number => scope + 900;

// A navigation: an HTTP GET or HEAD of a path whose last segment has no
// extension. Everything else (an asset miss, a POST) is the catch-all's.
const NAV_GUARD =
  '@src == "http" && (@web.req.method == "GET" || @web.req.method == "HEAD")\n' +
  "     && @web.req.url.path !~ /(?i)\\.[a-z0-9]+$/";

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
  const out: Record<string, string> = {};
  const nav = String(opts.scope);
  if (opts.shell !== null) {
    const known = knownRoutesRegex(routes);
    if (known === null) {
      out[`${nav}/spa-fallback.txcl`] = `# ${opts.producer} — SPA fallback (generated; do not edit by hand).
#
# Serves the app shell (${opts.fallbackName}) with 200 for any page navigation
# nothing else answered: the route table couldn't be expressed as a matcher,
# so unknown pages get 200 too and the client renders its not-found view.
# Regenerated on every build — the shell embeds content-hashed asset URLs.
WHEN ${NAV_GUARD}
${emitShell(200, opts.shell)}`;
    } else {
      out[`${nav}/spa-fallback.txcl`] = `# ${opts.producer} — SPA fallback: known routes (generated; do not edit by hand).
#
# Serves the app shell (${opts.fallbackName}) with 200 for a navigation to a
# client-rendered page route that no file or earlier op answered. The route
# set comes from SvelteKit's own route table. Paired with spa-404.txcl.
# Regenerated on every build — the shell embeds content-hashed asset URLs.
WHEN ${NAV_GUARD}
     && @web.req.url.path =~ /${known}/
${emitShell(200, opts.shell)}`;
      out[`${nav}/spa-404.txcl`] = `# ${opts.producer} — SPA 404 (generated; do not edit by hand).
#
# A navigation matching no known page route is a real miss: 404, with the
# shell as the body so the client renders its error page.
WHEN ${NAV_GUARD}
     && @web.req.url.path !~ /${known}/
${emitShell(404, opts.shell)}`;
    }
  }
  out[`${catchAllScope(opts.scope)}/not-found.txcl`] = `# ${opts.producer} — not found (generated; do not edit by hand).
#
# Every other HTTP request that reached the end of the stack: an asset that
# doesn't exist, a method nothing handles. Without it the request would get
# the envelope back as JSON. It sits after the navigation ops on purpose: ops
# in one scope run concurrently.
WHEN @src == "http"
  EMIT @web.res.status = 404,
       @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
       @web.res.body = b64"404 not found\\n",
       @halt = true
`;
  return out;
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

/** The EMIT tail for a shell answer: the shell as a readable b64"…" literal, and halt. */
export function emitShell(status: number, shell: string): string {
  // The lexer base64-encodes a b64"…" literal at parse time, so the file stays
  // readable HTML. Only backslash and double-quote need escaping.
  const escaped = shell.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  return `  EMIT @web.res.status = ${status},
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.headers.cache-control.0 = "no-cache",
       @web.res.body = b64"${escaped}",
       @halt = true
`;
}
