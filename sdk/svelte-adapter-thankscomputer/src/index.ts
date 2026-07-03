import type { Adapter, Builder } from "@sveltejs/kit";
import { existsSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const NAME = "@txco/svelte-adapter-thankscomputer";

export interface AdapterOptions {
  /**
   * The txco stack directory to write into — the one that holds `FILES/` and
   * scope folders (e.g. `OPS/web`). The build lands in `<out>/FILES/`.
   * Default: `OPS/web`.
   */
  out?: string;
  /**
   * SPA fallback page filename, or `false` to disable SPA mode (only the files
   * you prerender will be served). Default: `index.html`.
   */
  fallback?: string | false;
  /**
   * Generate `<out>/<fallbackScope>/spa-fallback.txcl`, an op that serves the
   * fallback shell for client-routed (extension-less) requests. Without it, a
   * hard reload of `/some/route` 404s. Default: `true`.
   */
  fallbackOp?: boolean;
  /**
   * txcl scope for the generated fallback op. Default: `900000`. The fallback is
   * a catch-all that halts, so it must run LAST — after `txco://route` and any
   * ops you add to this stack (APIs, redirects). A low scope would swallow
   * extension-less requests (`/api/x`) before your own handlers see them. The
   * default sits very high on purpose: it leaves the whole 1…899999 range free
   * for your own ops, so you never have to squeeze handlers under the catch-all.
   * (Empty scopes between your ops and this one cost nothing — the chassis
   * floor-jumps to the next populated scope.)
   */
  fallbackScope?: number;
  /**
   * Run `txco apply` (in the current directory) after writing the build, to
   * deploy in one step. Default: `false` — the adapter just prints the command.
   */
  apply?: boolean;
  /** Precompress assets with gzip + brotli. Default: `false`. */
  precompress?: boolean;
}

/**
 * SvelteKit adapter for thanks.computer (txco).
 *
 * It emits a fully static build into a stack's `FILES/` directory — served by
 * the built-in `txco://static` op with ETags and conditional GET — and
 * generates a small SPA-fallback op so client routes resolve on a hard reload.
 * Deploy the result with `txco apply`.
 */
export default function adapter(options: AdapterOptions = {}): Adapter {
  const {
    out = "OPS/web",
    fallback = "index.html",
    fallbackOp = true,
    fallbackScope = 900000,
    apply = false,
    precompress = false,
  } = options;

  return {
    name: NAME,

    async adapt(builder: Builder): Promise<void> {
      // The one hard requirement: txco never serves a request whose path has a
      // segment starting with "_" (a privacy convention — `FILES/_*` is readable
      // by ops but not over HTTP). SvelteKit's default appDir is `_app`, so every
      // hashed asset would silently 404. Fail loudly with the fix.
      const appDir = builder.config.kit.appDir;
      if (appDir.startsWith("_")) {
        throw new Error(
          `${NAME}: kit.appDir is "${appDir}", but thanks.computer never serves a ` +
            `path whose segment begins with "_", so every hashed asset under ` +
            `/${appDir}/ would 404. Set kit.appDir to a non-underscore name, e.g.\n\n` +
            `  kit: {\n    appDir: 'app',\n    adapter: adapter({ /* ... */ })\n  }\n`,
        );
      }

      const filesDir = join(out, "FILES");
      builder.rimraf(filesDir);
      builder.mkdirp(filesDir);

      builder.log.minor(`Writing client + prerendered output to ${filesDir}/`);
      // Client assets and prerendered pages share one tree; txco://static serves
      // any path under FILES/, so there's no pages/assets split.
      builder.writeClient(filesDir);
      builder.writePrerendered(filesDir);

      if (precompress) {
        builder.log.minor("Precompressing assets (gzip + brotli)");
        await builder.compress(filesDir);
      }

      let fallbackPath: string | null = null;
      if (fallback) {
        fallbackPath = join(filesDir, fallback);
        builder.log.minor(`Generating SPA fallback ${fallback}`);
        await builder.generateFallback(fallbackPath);
      }

      if (fallbackOp) {
        if (!fallbackPath) {
          builder.log.warn(
            `${NAME}: fallbackOp is on but fallback is disabled — skipping the SPA op. ` +
              `Client routes will 404 on reload unless every route is prerendered.`,
          );
        } else {
          writeFallbackOp(builder, out, fallbackScope, fallbackPath, fallback as string);
        }
      }

      builder.log.success(`Built thanks.computer stack at ${out}/`);

      if (apply) {
        runApply(builder, out);
      } else {
        builder.log.minor(`Next: deploy with \`txco apply\` (ships ${out}/ to your stack).`);
      }
    },
  };
}

/**
 * Write the SPA-fallback op. txco://static serves real files (assets, the
 * fallback page) at boot/50 and falls through for extension-less paths; this op
 * serves the shell for those so deep links / reloads render the app.
 *
 * The shell is embedded as a base64 string — the canonical desugared form of a
 * web body (the web inlet base64-decodes `_txc.web.res.body`). It's regenerated
 * on every build because the shell references content-hashed asset URLs.
 */
function writeFallbackOp(
  builder: Builder,
  out: string,
  scope: number,
  fallbackPath: string,
  fallbackName: string,
): void {
  const b64 = readFileSync(fallbackPath).toString("base64");

  // Remove any stale generated fallback ops from previous builds (any scope), so
  // exactly the intended files remain. A leftover at a lower scope would fire
  // first and halt, silently defeating the current scope / route-aware behavior.
  const generated = ["spa-fallback.txcl", "spa-404.txcl"];
  if (existsSync(out)) {
    for (const entry of readdirSync(out, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      for (const name of generated) {
        const stale = join(out, entry.name, name);
        if (existsSync(stale)) rmSync(stale);
      }
    }
  }

  const opDir = join(out, String(scope));
  builder.mkdirp(opDir);

  // The WHENs fire only for navigations with no file extension. These ops live at
  // a high (last-resort) scope so they run AFTER txco://route and any other ops in
  // this stack — and halt — so they never swallow an extension-less request that
  // an earlier-scope handler (e.g. an API) should answer.
  const extGuard = "@web.req.url.path !~ /\\.[a-z0-9]+$/";
  const known = knownRoutesRegex(builder.routes);

  if (!known) {
    // Fail open: no representable page routes (or a route uses a regex feature Go's
    // RE2 lacks) → the legacy blanket-200 catch-all. Never worse than before, and
    // never 404s a real route.
    builder.log.warn(
      `${NAME}: couldn't derive a route matcher from builder.routes — emitting the ` +
        `legacy blanket-200 SPA fallback (unknown paths return 200, not 404).`,
    );
    const opPath = join(opDir, "spa-fallback.txcl");
    writeFileSync(
      opPath,
      `# ${NAME} — SPA fallback (generated; do not edit by hand).
#
# Serves the SvelteKit shell for any extension-less path nothing else handled.
# @web.res.body is base64-encoded ${fallbackName} (the web inlet decodes it).
# Regenerated on every \`vite build\` — it embeds content-hashed asset URLs.
WHEN ${extGuard}
${emitShell(200, b64)}`,
    );
    builder.log.minor(`Wrote SPA fallback op ${opPath} (blanket 200)`);
    return;
  }

  // Route-aware: 200 for paths matching a known page route (the shell, so the
  // client renders), 404 for extension-less paths that match no route at all.
  // <known> is derived from SvelteKit's own route table, so it tracks route
  // changes automatically. Two mutually-exclusive ops (one WHEN each, per the txcl
  // convention) — the concrete + dynamic page routes share this same matcher.
  const pageRoutes = builder.routes.filter(
    (r) => r.page && r.page.methods.length > 0,
  ).length;

  writeFileSync(
    join(opDir, "spa-fallback.txcl"),
    `# ${NAME} — SPA fallback: known routes (generated; do not edit by hand).
#
# txco://static serves real files (assets, ${fallbackName}) at boot/50 and halts.
# This op serves the SvelteKit shell (200) for client-rendered PAGE routes that no
# static file or earlier op handled. The route set is derived from SvelteKit's own
# route table (builder.routes[].pattern). Paired with spa-404.txcl, which 404s
# extension-less paths matching no route.
# @web.res.body is base64-encoded ${fallbackName}. Regenerated on every build.
WHEN ${extGuard} && @web.req.url.path =~ /${known}/
${emitShell(200, b64)}`,
  );

  writeFileSync(
    join(opDir, "spa-404.txcl"),
    `# ${NAME} — SPA 404 (generated; do not edit by hand).
#
# An extension-less path matching NO known page route (and handled by no static
# file or earlier op) is a genuine miss → HTTP 404. The shell is still served as
# the body so the client renders its branded error page; only the status differs
# from spa-fallback.txcl.
WHEN ${extGuard} && @web.req.url.path !~ /${known}/
${emitShell(404, b64)}`,
  );

  builder.log.minor(
    `Wrote route-aware SPA fallback in ${opDir}/ (200 for ${pageRoutes} page routes, 404 otherwise)`,
  );
}

/**
 * Build an anchored regex STRING matching exactly the SvelteKit PAGE routes,
 * derived from the framework's own route table (`builder.routes[].pattern`).
 * Returns null when there are no page routes, or when a route's pattern uses a
 * regex feature Go's RE2 engine (which txcl compiles with) can't represent — the
 * caller then falls back to the legacy blanket-200 op rather than risk 404-ing a
 * real route. Literal slashes are emitted escaped (`\\/`) for the txcl `/.../`
 * delimiter; the txcl lexer unescapes them to `/` before RE2 sees the pattern.
 */
function knownRoutesRegex(routes: Builder["routes"]): string | null {
  const parts: string[] = [];
  for (const r of routes) {
    // Renderable pages only (page.methods non-empty); skip +server.ts / api-only.
    if (!r.page || r.page.methods.length === 0) continue;
    let src = r.pattern.source; // e.g. "^\\/me\\/drips\\/([^/]+?)\\/?$"
    // RE2 has no lookaround or backreferences; if a route needs them, bail.
    if (/\(\?[=!<]/.test(src) || /\\[1-9]/.test(src)) return null;
    src = src.replace(/^\^/, "").replace(/\$$/, ""); // strip anchors
    src = src.replace(/\\?\//g, "\\/"); // normalize every literal slash for /.../
    parts.push(`(?:${src})`);
  }
  return parts.length ? `^(?:${parts.join("|")})$` : null;
}

/** Shared EMIT tail for the fallback ops: headers, the base64 shell, and halt. */
function emitShell(status: number, b64: string): string {
  return `  EMIT @web.res.status = ${status},
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.headers.cache-control.0 = "no-cache",
       @web.res.body = "${b64}",
       @halt = true
`;
}

/** Deploy by shelling out to `txco apply` in the current working directory. */
function runApply(builder: Builder, out: string): void {
  builder.log.minor("Running `txco apply`");
  const res = spawnSync("txco", ["apply"], { stdio: "inherit" });
  if (res.error) {
    builder.log.warn(
      `txco apply could not start (${res.error.message}); run it yourself from the ` +
        `workspace that contains ${out}/.`,
    );
  } else if (res.status !== 0) {
    builder.log.warn(`txco apply exited with code ${res.status}; see the output above.`);
  } else {
    builder.log.success("Deployed via txco apply.");
  }
}
