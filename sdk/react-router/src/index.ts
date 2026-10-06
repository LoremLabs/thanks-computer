// @txco/react-router: a React Router preset that writes a Web ABI build for
// Thanks, Computer (txco) after `react-router build`:
//
//   txco-web/
//     txco-web.json   the manifest
//     public/         a copy of build/client
//     ops/            the route-aware SPA fallback and the catch-all
//
//   // react-router.config.ts
//   import type { Config } from "@react-router/dev/config";
//   import txco from "@txco/react-router";
//   export default { ssr: false, presets: [txco()] } satisfies Config;
//
// `txco apply` installs it into the stack txco.yaml binds it to.
import { readdir } from "node:fs/promises";
import { resolve } from "node:path";

import { checkOutDir } from "@txco/web-abi/producer";

import { NAME, writeBuild, type TxcoOptions } from "./build.js";
import type { RouteManifest } from "./routes.js";

export type { TxcoOptions } from "./build.js";
export { routeRegex, routesMatcher } from "./routes.js";

// The parts of React Router's types the preset reads, declared here so the
// package needs no React Router at build time. They match @react-router/dev's
// Preset and BuildEndHook (v7 and v8).
interface ResolvedConfig {
  buildDirectory: string;
  basename: string;
  ssr: boolean;
  routes: RouteManifest;
}
interface BuildEndArgs {
  buildManifest?: { routes: RouteManifest };
  reactRouterConfig: ResolvedConfig;
  viteConfig: {
    root: string;
    base: string;
    build: { assetsDir: string };
    logger: { info(msg: string): void; warn(msg: string): void };
  };
}
export interface Preset {
  name: string;
  reactRouterConfig?: (args: { reactRouterUserConfig: unknown }) => { buildEnd: (args: BuildEndArgs) => Promise<void> };
  reactRouterConfigResolved?: (args: { reactRouterConfig: ResolvedConfig }) => Promise<void>;
}

export default function txco(options: TxcoOptions = {}): Preset {
  return {
    name: NAME,

    // Fail before a long build: the preset wipes and rewrites `out`.
    async reactRouterConfigResolved({ reactRouterConfig }) {
      const root = process.cwd();
      const out = resolve(root, options.out ?? "txco-web");
      const build = resolve(root, reactRouterConfig.buildDirectory);
      const problems = checkOutDir({
        dir: out,
        protect: [root, build],
        notInside: [build],
        entries: await readdir(out).catch(() => null),
      });
      if (problems.length > 0) throw new Error(`[txco] ${problems.join("\n")}`);
    },

    reactRouterConfig: () => ({
      async buildEnd({ buildManifest, reactRouterConfig, viteConfig }) {
        await writeBuild({
          root: viteConfig.root,
          buildDirectory: reactRouterConfig.buildDirectory,
          base: viteConfig.base,
          assetsDir: viteConfig.build.assetsDir,
          basename: reactRouterConfig.basename,
          ssr: reactRouterConfig.ssr,
          routes: buildManifest?.routes ?? reactRouterConfig.routes,
          options,
          log: viteConfig.logger,
        });
      },
    }),
  };
}
