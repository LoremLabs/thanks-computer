// @txco/astro: an Astro integration that writes a Web ABI build for Thanks,
// Computer (txco) after `astro build`:
//
//   txco-web/
//     txco-web.json   the manifest
//     public/         a copy of the built site
//     ops/            the 404 page and the catch-all
//
//   // astro.config.mjs
//   import txco from "@txco/astro";
//   export default defineConfig({ integrations: [txco()] });
//
// `txco apply` installs it into the stack txco.yaml binds it to.
import { readdir } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { checkOutDir } from "@txco/web-abi/producer";

import { NAME, writeBuild, type TxcoOptions } from "./build.js";

export type { TxcoOptions } from "./build.js";

// The parts of Astro's integration API this reads, declared here so the
// package needs no Astro at build time (they match Astro 5–7).
interface Logger {
  info(msg: string): void;
  warn(msg: string): void;
}
interface ConfigLike {
  root: URL;
  srcDir: URL;
  publicDir: URL;
  outDir: URL;
  base: string;
  output?: string;
  build: { assets: string };
}
export interface AstroIntegration {
  name: string;
  hooks: {
    "astro:config:done"?: (opts: { config: ConfigLike; buildOutput?: "static" | "server"; logger: Logger }) => Promise<void>;
    "astro:build:done"?: (opts: { dir: URL; pages: { pathname: string }[]; logger: Logger }) => Promise<void>;
  };
}

export default function txco(options: TxcoOptions = {}): AstroIntegration {
  let config: ConfigLike | undefined;
  let output = "static";
  let out = "";

  return {
    name: NAME,
    hooks: {
      // Fail before the build: the integration wipes and rewrites `out`.
      async "astro:config:done"({ config: c, buildOutput }) {
        config = c;
        output = buildOutput ?? (c.output === "server" ? "server" : "static");
        const root = fileURLToPath(c.root);
        const outDir = fileURLToPath(c.outDir);
        out = resolve(root, options.out ?? "txco-web");
        const problems = checkOutDir({
          dir: out,
          protect: [root, fileURLToPath(c.srcDir), fileURLToPath(c.publicDir), outDir],
          notInside: [outDir],
          entries: await readdir(out).catch(() => null),
        });
        if (problems.length > 0) throw new Error(`[${NAME}] ${problems.join("\n")}`);
      },

      async "astro:build:done"({ dir, pages, logger }) {
        if (!config) throw new Error(`[${NAME}] astro:build:done ran before astro:config:done`);
        await writeBuild({
          dir: fileURLToPath(dir),
          out,
          base: config.base,
          assets: config.build.assets,
          output,
          pages: pages.length,
          options,
          log: logger,
        });
      },
    },
  };
}
