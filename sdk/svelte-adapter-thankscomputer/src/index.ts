import type { Adapter, Builder } from "@sveltejs/kit";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { join, resolve, sep } from "node:path";
import { spawnSync } from "node:child_process";

import { checkOutDir, writeManifest, writeOps } from "@txco/web-abi/producer";

import { renderOps } from "./ops.js";

export { renderOps, knownRoutesRegex, catchAllScope } from "./ops.js";

const NAME = "@txco/svelte-adapter-thankscomputer";
const VERSION = "0.3.1";

export interface AdapterOptions {
  /**
   * The Web ABI directory to write: `txco-web.json`, `public/` and `ops/`.
   * Bind it to a stack in txco.yaml (`stacks: { web: { abi: <out> } }`).
   * It is wiped and rewritten on every build, so it must lie outside OPS/.
   * Default: `txco-web`.
   */
  out?: string;
  /**
   * SPA fallback page filename, or `false` to disable SPA mode (only the files
   * you prerender are served; other navigations get a plain 404).
   * Default: `index.html`.
   */
  fallback?: string | false;
  /**
   * Write the navigation ops (spa-fallback, spa-404) that serve the fallback
   * shell for client routes. Without them a hard reload of a client route
   * 404s. Default: `true`.
   */
  fallbackOp?: boolean;
  /**
   * The scope for the generated navigation ops; the catch-all goes 900
   * above it. Default: `900000`, the Web ABI producer band, after every op an
   * author writes.
   */
  fallbackScope?: number;
  /**
   * Run `txco apply` (in the current directory) after the build, to deploy in
   * one step. Default: `false`.
   */
  apply?: boolean;
  /**
   * Deprecated and ignored: nothing serves precompressed copies (the edge
   * compresses).
   */
  precompress?: boolean;
}

/**
 * SvelteKit adapter for thanks.computer (txco).
 *
 * It writes a Web ABI build: the client and prerendered output as `public/`,
 * the ops a SvelteKit SPA needs (a route-aware fallback and a catch-all) as
 * `ops/`, and `txco-web.json`. Bind it to a stack in txco.yaml and deploy
 * with `txco apply` / `txco push`; check it first with `txco web check`.
 */
export default function adapter(options: AdapterOptions = {}): Adapter {
  const {
    out = "txco-web",
    fallback = "index.html",
    fallbackOp = true,
    fallbackScope = 900000,
    apply = false,
    precompress = false,
  } = options;

  return {
    name: NAME,

    async adapt(builder: Builder): Promise<void> {
      guardOut(out);
      if (precompress) {
        builder.log.warn(`${NAME}: precompress is deprecated and ignored — the edge compresses responses.`);
      }

      builder.rimraf(out);
      const pub = join(out, "public");
      builder.mkdirp(pub);
      builder.log.minor(`Writing client + prerendered output to ${pub}/`);
      builder.writeClient(pub);
      builder.writePrerendered(pub);
      warnDotPaths(builder, pub);

      let shell: string | null = null;
      if (fallback && fallbackOp) {
        const fallbackPath = join(pub, fallback);
        builder.log.minor(`Generating SPA fallback ${fallback}`);
        await builder.generateFallback(fallbackPath);
        shell = readFileSync(fallbackPath, "utf8");
      } else if (fallback) {
        await builder.generateFallback(join(pub, fallback));
      }

      const ops = renderOps(builder.routes, {
        scope: fallbackScope,
        shell,
        fallbackName: fallback || "",
        producer: NAME,
      });
      await writeOps(out, ops);

      const appDir = builder.config.kit.appDir;
      await writeManifest(out, {
        abi: 1,
        immutable: [`${appDir}/immutable/`],
        "x-producer": { name: NAME, version: VERSION },
      });

      builder.log.success(`Built a Web ABI build at ${out}/ (${Object.keys(ops).length} ops)`);
      if (apply) {
        runApply(builder, out);
      } else {
        builder.log.minor(`Next: \`txco web check ${out}\`, then deploy with \`txco apply\` (bind ${out} to a stack in txco.yaml).`);
      }
    },
  };
}

/**
 * Refuses an `out` that isn't a Web ABI directory: one inside OPS/ (where the
 * walker would read it as a stack, and where 0.2 wrote its output), one
 * holding the project, or an existing directory holding anything a build
 * doesn't write. 0.3 wipes `out` on every build, so a 0.2 setting
 * (`out: 'OPS/web'`) must never reach it.
 */
function guardOut(out: string): void {
  const abs = resolve(out);
  if (abs.split(sep).includes("OPS")) {
    throw new Error(
      `${NAME}: out is "${out}", inside OPS/. Since 0.3 the adapter writes a Web ABI build ` +
        `that it wipes on every build, so it must live outside OPS/ — e.g. out: 'txco-web' — ` +
        `and be bound to the stack in txco.yaml:\n\n  stacks:\n    web:\n      abi: www/txco-web\n`,
    );
  }
  const problems = checkOutDir({
    dir: abs,
    protect: [process.cwd()],
    entries: existsSync(abs) ? readdirSync(abs) : null,
  });
  if (problems.length > 0) throw new Error(`${NAME}: ${problems.join("\n")}`);
}

/** Dot paths in public/ never deploy (the installer skips them): say so. */
function warnDotPaths(builder: Builder, pub: string): void {
  const found: string[] = [];
  const walk = (dir: string, rel: string) => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const r = rel ? `${rel}/${e.name}` : e.name;
      if (e.name.startsWith(".")) found.push(r);
      else if (e.isDirectory()) walk(join(dir, e.name), r);
    }
  };
  walk(pub, "");
  if (found.length > 0) {
    builder.log.warn(`${NAME}: ${found.length} dot path(s) in the build never deploy: ${found.slice(0, 3).join(", ")}`);
  }
}

/** Deploy by shelling out to `txco apply` in the current working directory. */
function runApply(builder: Builder, out: string): void {
  builder.log.minor("Running `txco apply`");
  const res = spawnSync("txco", ["apply"], { stdio: "inherit" });
  if (res.error) {
    builder.log.warn(
      `txco apply could not start (${res.error.message}); run it yourself from the workspace ` +
        `whose txco.yaml binds ${out}.`,
    );
  } else if (res.status !== 0) {
    builder.log.warn(`txco apply exited with code ${res.status}; see the output above.`);
  } else {
    builder.log.success("Deployed via txco apply.");
  }
}
