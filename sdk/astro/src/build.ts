// Writes the Web ABI build from Astro's finished static output. Called from
// the integration's astro:build:done; takes plain inputs, so it's tested
// without a build.
import { readFile, rm } from "node:fs/promises";
import { join, relative } from "node:path";

import { copyPublic, listFiles, looksHashed, publicPrefix, renderOps, writeManifest, writeOps } from "@txco/web-abi/producer";

export const NAME = "@txco/astro";
export const VERSION = "0.1.0";
/** An op answer is capped at 4 MiB (--op-payload-max), base64 included. */
const PAGE_WARN_BYTES = 1 << 20;

export interface TxcoOptions {
  /** The Web ABI directory to write, relative to the project root. Default "txco-web". */
  out?: string;
  /** The public/ prefixes cached for a year. Default: Astro's build.assets (`_astro/`). */
  immutable?: string[];
  /** The navigation op's scope; the catch-all goes 900 above it. Default 900000. */
  scope?: number;
}

export interface BuildInput {
  /** The finished static output: outDir, or build.client for a server build. */
  dir: string;
  /** The Web ABI directory, absolute. */
  out: string;
  /** Astro's base (where the site is served; not part of the file paths). */
  base: string;
  /** Astro's build.assets. */
  assets: string;
  /** "static" or "server". */
  output: string;
  /** How many pages Astro wrote. */
  pages: number;
  options: TxcoOptions;
  log: { info(msg: string): void; warn(msg: string): void };
}

export async function writeBuild(i: BuildInput): Promise<{ ops: string[]; manifest: Record<string, unknown> }> {
  const warn = i.log.warn;
  const { prefix, external } = publicPrefix(i.base);
  if (external) warn(`base is ${i.base}: the pages are deployed at the root`);

  // Astro writes the site without its base; the files go under public/<base>/.
  await rm(i.out, { recursive: true, force: true });
  const { skipped } = await copyPublic(i.dir, i.out, prefix);
  if (skipped.length > 0) warn(`the build has ${skipped.length} dot path(s) (${skipped.slice(0, 3).join(", ")}); they never deploy`);

  if (i.output === "server") {
    warn("output: 'server' needs a server, and no chassis runs one yet: only the prerendered pages deploy.");
  }

  // An Astro site is multi-page: a path with no page gets 404.html.
  const html = await readFile(join(i.dir, "404.html"), "utf8").catch(() => null);
  const page = html === null ? null : { name: "404.html", html };
  if (page && Buffer.byteLength(page.html) > PAGE_WARN_BYTES) {
    warn(`404.html is ${Buffer.byteLength(page.html)} bytes; the op that serves it is capped at 4 MiB, base64 included`);
  }
  const ops = renderOps({
    mode: "404",
    scope: i.options.scope,
    producer: NAME,
    page,
    notes: page ? [] : ["Add src/pages/404.astro to serve your own."],
  });
  await writeOps(i.out, ops);

  const files = await listFiles(join(i.out, "public"));
  const assets = i.assets.replace(/^\/+|\/+$/g, "");
  const immutable =
    i.options.immutable ?? (assets && files.some((f) => f.startsWith(`${prefix}${assets}/`)) ? [`${prefix}${assets}/`] : []);
  for (const p of immutable) {
    const unhashed = files.filter((f) => f.startsWith(p) && !looksHashed(f.split("/").pop()!));
    if (unhashed.length > 0) {
      warn(`${unhashed.length} file(s) under the immutable prefix ${p} carry no content hash (${unhashed.slice(0, 3).join(", ")}), so a change to them wouldn't reach a browser for a year`);
    }
  }

  const manifest: Record<string, unknown> = {
    abi: 1,
    ...(immutable.length > 0 ? { immutable } : {}),
    "x-producer": { name: NAME, version: VERSION },
    "x-astro": { output: i.output, pages: i.pages },
  };
  await writeManifest(i.out, manifest);

  const shown = relative(process.cwd(), i.out) || ".";
  i.log.info(
    `Web ABI build at ${shown}: ${files.length} public file(s), ${Object.keys(ops).length} op(s). ` +
      `Next: \`txco web check ${shown}\`, bind it in txco.yaml (stacks: <name>: abi: ${shown}), then \`txco apply\`.`,
  );
  return { ops: Object.keys(ops).sort(), manifest };
}
