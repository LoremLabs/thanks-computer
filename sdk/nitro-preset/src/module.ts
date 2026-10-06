// The preset's Nitro module. Nitro runs modules inside createNitro, before
// prepare() wipes the output dir, so this is where the guard goes; it also
// registers the hooks that finish the build.
import { readdir } from "node:fs/promises";

import type { Nitro } from "nitropack/types";

import { finalize, PRESET, type FinalizeNitro } from "./finalize.js";
import { checkOutDir } from "./guard.js";

export const thanksComputerModule = {
  name: PRESET,
  async setup(nitro: Nitro): Promise<void> {
    const o = nitro.options;
    const entries = await readdir(o.output.dir).catch(() => null);
    const problems = checkOutDir({
      dir: o.output.dir,
      publicDir: o.output.publicDir,
      serverDir: o.output.serverDir,
      baseURL: o.baseURL,
      protect: [o.rootDir, o.srcDir, o.buildDir, ...o.publicAssets.map((a) => a.dir)],
      entries,
    });
    if (problems.length > 0) throw new Error(`[${PRESET}] ${problems.join("\n")}`);

    // A static build (nuxi generate) crawls from the home page, as Nitro's
    // `static` preset does: Nuxt seeds "/" only when crawlLinks is on, and
    // reads it in nitro:init, after this module. An explicit setting stands.
    const asked = (o._config as { prerender?: { crawlLinks?: boolean } } | undefined)?.prerender?.crawlLinks;
    if (o.static && asked === undefined) o.prerender.crawlLinks = true;

    // Hooks registered here, not in the preset's `hooks`: the config merge
    // lets a user's own `hooks.compiled` replace a preset's.
    let finalized = false;
    let prerendered = false;
    const run = async (server: boolean) => {
      finalized = true;
      await finalize(nitro as unknown as FinalizeNitro, { server });
    };
    nitro.hooks.hook("prerender:done", ({ failedRoutes }) => {
      prerendered = !(o.prerender.failOnError && failedRoutes.length > 0);
    });
    nitro.hooks.hook("compiled", async () => {
      if (!finalized) await run(!o.static);
    });
    // A framework that prerenders and never calls build() (Analog's static
    // build) never fires `compiled`; it always closes. Without a finished
    // prerender (nuxi prepare, a failed build) there is nothing to finish.
    nitro.hooks.hook("close", async () => {
      if (!finalized && prerendered) await run(false);
    });
  },
};
