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
   * txcl scope for the generated fallback op. Default: `1000`. The fallback is
   * a catch-all that halts, so it must run LAST — after `txco://route` and any
   * ops you add to this stack (APIs, redirects). A low scope would swallow
   * extension-less requests (`/api/x`) before your own handlers see them. 1000
   * matches the house last-resort convention (`_sys/boot/1000` notfound).
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
    fallbackScope = 1000,
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

  // Remove any stale spa-fallback op from a previous build at a different
  // scope, so exactly one exists. Otherwise a leftover at a lower scope would
  // fire first and halt — silently defeating the current fallbackScope.
  if (existsSync(out)) {
    for (const entry of readdirSync(out, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      const stale = join(out, entry.name, "spa-fallback.txcl");
      if (existsSync(stale)) rmSync(stale);
    }
  }

  const opDir = join(out, String(scope));
  builder.mkdirp(opDir);
  const opPath = join(opDir, "spa-fallback.txcl");

  // The WHEN fires only for navigations with no file extension. This op lives
  // at a high (last-resort) scope so it runs AFTER txco://route and any other
  // ops in this stack — and halts — so it never swallows an extension-less
  // request that an earlier-scope handler (e.g. an API) should answer.
  const txcl = `# ${NAME} — SPA fallback (generated; do not edit by hand).
#
# txco://static serves real files (assets, ${fallbackName}) at boot/50 and falls
# through for extension-less paths (client routes and "/"). This op — a catch-all
# at the stack's last scope — serves the SvelteKit shell for those so a hard
# reload or deep link renders the app. Being last + halting means it only fires
# for requests nothing else (static, your own ops) handled.
#
# @web.res.body is base64-encoded ${fallbackName} (the web inlet decodes it).
# Regenerated on every \`vite build\` — it embeds content-hashed asset URLs.
WHEN @web.req.url.path !~ /\\.[a-z0-9]+$/
  EMIT @web.res.status = 200,
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.headers.cache-control.0 = "no-cache",
       @web.res.body = "${b64}",
       @halt = true
`;

  writeFileSync(opPath, txcl);
  builder.log.minor(`Wrote SPA fallback op ${opPath}`);
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
