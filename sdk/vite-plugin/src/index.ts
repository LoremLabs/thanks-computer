// @txco/vite-plugin: after `vite build`, writes a Web ABI build for Thanks,
// Computer (txco) beside your dist/:
//
//   txco-web/
//     txco-web.json   the manifest
//     public/         a copy of dist/
//     ops/            the navigation op and the catch-all
//
//   // vite.config.ts
//   import txco from "@txco/vite-plugin";
//   export default defineConfig({ plugins: [txco()] });
//
// `txco apply` installs it into the stack txco.yaml binds it to.
import { readdir, readFile, rm } from "node:fs/promises";
import { join, relative, resolve } from "node:path";

import {
  checkOutDir,
  copyPublic,
  listFiles,
  looksHashed,
  publicPrefix,
  renderOps,
  writeManifest,
  writeOps,
} from "@txco/web-abi/producer";
import type { Plugin, ResolvedConfig } from "vite";

import { chooseMode, shellPage } from "./build.js";

export const NAME = "@txco/vite-plugin";
export const VERSION = "0.1.0";
/** An op answer is capped at 4 MiB (--op-payload-max), base64 included. */
const PAGE_WARN_BYTES = 1 << 20;

export interface TxcoOptions {
  /** The Web ABI directory to write, relative to the project root. Default "txco-web". */
  out?: string;
  /**
   * Answer every unknown page path with the shell (index.html) and 200. The
   * default: on for a one-page build, off for a multi-page one.
   */
  spa?: boolean;
  /** The public/ prefixes cached for a year. Default: Vite's assetsDir (`assets/`). */
  immutable?: string[];
  /** The navigation op's scope; the catch-all goes 900 above it. Default 900000. */
  scope?: number;
}

export default function txco(options: TxcoOptions = {}): Plugin {
  let config: ResolvedConfig;
  let out = "";
  let skip = false;

  return {
    name: "txco",
    apply: "build",
    enforce: "post",
    // One Web ABI build, from the client build (Vite ≥ 6 asks per environment).
    applyToEnvironment: (env) => env.name === "client",

    async configResolved(c) {
      config = c;
      if (c.build.lib || c.build.ssr) {
        skip = true;
        return;
      }
      out = resolve(c.root, options.out ?? "txco-web");
      const outDir = resolve(c.root, c.build.outDir);
      // Before anything is written: the build wipes and rewrites `out`.
      const problems = checkOutDir({
        dir: out,
        protect: [c.root, outDir, c.publicDir].filter(Boolean),
        notInside: [outDir],
        entries: await readdir(out).catch(() => null),
      });
      if (problems.length > 0) throw new Error(`[txco] ${problems.join("\n")}`);
    },

    writeBundle: {
      order: "post",
      sequential: true,
      async handler(_output, bundle) {
        if (skip) return;
        const log = config.logger;
        const warn = (msg: string) => log.warn(`[txco] ${msg}`);
        const outDir = resolve(config.root, config.build.outDir);

        const pages = Object.values(bundle)
          .filter((o) => o.type === "asset" && o.fileName.endsWith(".html"))
          .map((o) => o.fileName)
          .sort();
        const mode = chooseMode(pages, options.spa);
        const { prefix, external } = publicPrefix(config.base);
        if (external) warn(`base is ${config.base}: the assets load from there, and the pages are deployed at the root`);

        // dist/ as public/, without what never deploys: .vite/ (build
        // metadata) quietly, any other dot path with a warning.
        await rm(out, { recursive: true, force: true });
        const { skipped: dots } = await copyPublic(outDir, out, prefix);
        if (dots.length > 0) warn(`dist/ has ${dots.length} dot path(s) (${dots.slice(0, 3).join(", ")}); they never deploy`);

        const readPage = async (name: string) => {
          const html = await readFile(join(outDir, name), "utf8").catch(() => null);
          return html === null ? null : { name, html };
        };
        let page;
        if (mode === "spa") {
          const name = shellPage(pages);
          page = name ? await readPage(name) : null;
          if (!page) throw new Error(`[txco] a single-page build needs its shell (index.html), and ${outDir} has none`);
        } else {
          page = await readPage("404.html");
        }
        if (page && Buffer.byteLength(page.html) > PAGE_WARN_BYTES) {
          warn(`${page.name} is ${Buffer.byteLength(page.html)} bytes; the op that serves it is capped at 4 MiB, base64 included`);
        }

        const ops = renderOps({
          mode: mode === "spa" ? "spa" : "404",
          scope: options.scope,
          producer: NAME,
          page,
          notes: [
            mode === "spa"
              ? "A Vite app routes in the browser and has no route table to check."
              : `This build has ${pages.length} pages, each a file in public/.`,
          ],
        });
        await writeOps(out, ops);

        const files = await listFiles(join(out, "public"));
        const assets = config.build.assetsDir.replace(/^\/+|\/+$/g, "");
        const immutable = options.immutable ?? (assets && files.some((f) => f.startsWith(`${prefix}${assets}/`)) ? [`${prefix}${assets}/`] : []);
        for (const p of immutable) {
          const unhashed = files.filter((f) => f.startsWith(p) && !looksHashed(f.split("/").pop()!));
          if (unhashed.length > 0) {
            warn(`${unhashed.length} file(s) under the immutable prefix ${p} carry no content hash (${unhashed.slice(0, 3).join(", ")}), so a change to them wouldn't reach a browser for a year`);
          }
        }

        await writeManifest(out, {
          abi: 1,
          ...(immutable.length > 0 ? { immutable } : {}),
          "x-producer": { name: NAME, version: VERSION },
          "x-vite": { version: this.meta.viteVersion, mode, pages: pages.length },
        });

        const shown = relative(process.cwd(), out) || ".";
        log.info(
          `[txco] Web ABI build (${mode}) at ${shown}: ${files.length} public file(s), ${Object.keys(ops).length} op(s). ` +
            `Next: \`txco web check ${shown}\`, bind it in txco.yaml (stacks: <name>: abi: ${shown}), then \`txco apply\`.`,
        );
      },
    },
  };
}

export { chooseMode, publicPrefix } from "./build.js";
