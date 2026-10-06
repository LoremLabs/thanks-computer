// The ops this preset writes into a Web ABI build's ops/. Pure: no filesystem
// and no Nitro runtime, so the output is golden-tested. The rules come from
// @txco/web-abi/producer, which every producer shares; this file maps the
// preset's build modes onto them.
import { BUILTIN_404, catchAllScope, NAV_GUARD, renderOps as renderABIOps, type Page } from "@txco/web-abi/producer";

export { BUILTIN_404, catchAllScope, NAV_GUARD, type Page };

/**
 * How the build answers a page navigation nothing else answered:
 *
 *  - "static": every page was prerendered into public/, so a path that
 *    reaches the ops has no page: 404, with the 404 page;
 *  - "spa": an `ssr: false` app routes in the browser: 200, with the shell;
 *  - "server": the build has a server (server/). No chassis runs one yet, so
 *    a --static-only install answers like "static".
 */
export type Mode = "static" | "spa" | "server";

export interface RenderOptions {
  /** The producer band's scope for the navigation op (900000). */
  scope: number;
  mode: Mode;
  /**
   * The page the navigation op serves: 404.html (static, server) or the
   * shell (spa). null in static or server mode writes a small built-in 404
   * page instead. spa mode needs one.
   */
  page: Page | null;
  /** The generated-file header's producer name. */
  producer: string;
}

const NOTES: Record<Mode, string[]> = {
  spa: ["An ssr: false app: the client router renders every page."],
  static: ["A client route that wasn't prerendered still renders: Nuxt's 404.html", "is its app shell."],
  server: [
    "This build has a server (server/), and no chassis runs one yet: a",
    "--static-only install serves its prerendered pages, and every other page",
    "navigation ends here. A dispatch op replaces this one once a runner exists.",
  ],
};

/**
 * Renders the build's ops, as path (relative to ops/) → file text: one
 * navigation op for the mode, and the catch-all, a plain 404 for every other
 * HTTP request, so every request reaching the end of the stack is answered.
 */
export function renderOps(opts: RenderOptions): Record<string, string> {
  return renderABIOps({
    scope: opts.scope,
    producer: opts.producer,
    mode: opts.mode === "spa" ? "spa" : "404",
    page: opts.page,
    notes: NOTES[opts.mode],
  });
}
