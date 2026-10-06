// Turns Nitro's output dir into a Web ABI build: the ops, the manifest, and
// the cleanup. It runs once, after Nitro has written public/ (and server/).
import { readFile, rm, stat } from "node:fs/promises";
import { join, relative, sep } from "node:path";

import { listFiles, writeManifest, writeOps } from "@txco/web-abi/producer";

import { checkImmutable, deriveImmutable, looksHashed, type AssetDir } from "./immutable.js";
import { renderOps, type Mode, type Page } from "./ops.js";
import type { ThanksComputerOptions } from "./types.js";

export const NAME = "@txco/nitro-preset";
export const VERSION = "0.1.0";
export const PRESET = "thanks-computer";
export const MANIFEST = "txco-web.json";
/** The producer band (chassis/webabi ProducerScope). */
export const PRODUCER_SCOPE = 900000;
/** An op answer is capped at 4 MiB (--op-payload-max), base64 included. */
const PAGE_WARN_BYTES = 1 << 20;

/** The part of a Nitro instance finalize reads. A `Nitro` from nitropack 2 is one. */
export interface FinalizeNitro {
  options: {
    static: boolean;
    baseURL: string;
    output: { dir: string; publicDir: string; serverDir: string };
    publicAssets: readonly AssetDir[];
    routeRules: Record<string, { ssr?: boolean } | undefined>;
    renderer?: string;
    framework: { name?: string; version?: string };
    compressPublicAssets?: unknown;
    rollupConfig?: { output?: unknown };
    thanksComputer?: ThanksComputerOptions;
  };
  _prerenderedRoutes?: readonly { route: string }[];
  logger: { info(...args: unknown[]): void; warn(...args: unknown[]): void; success(...args: unknown[]): void };
}

export interface FinalizeResult {
  mode: Mode;
  manifest: Record<string, unknown>;
  ops: string[];
  warnings: string[];
}

const exists = (p: string) => stat(p).then(() => true, () => false);
const posix = (p: string) => p.split(sep).join("/");

/** Whether the app routes in the browser: the option, or what Nuxt and Analog tell us. */
export function isSpa(nitro: FinalizeNitro): boolean {
  const forced = nitro.options.thanksComputer?.spa;
  if (typeof forced === "boolean") return forced;
  // Nuxt prerenders /index.html as the shell only for an ssr: false app;
  // with component islands it turns that into a routeRule instead.
  if (nitro._prerenderedRoutes?.some((r) => r.route === "/index.html")) return true;
  if (nitro.options.routeRules?.["/**"]?.ssr === false) return true;
  // Analog's ssr: false renderer.
  return typeof nitro.options.renderer === "string" && /CLIENT_RENDERER/.test(nitro.options.renderer);
}

/**
 * Writes the build's ops and manifest into Nitro's output dir and removes
 * what isn't part of a Web ABI build. `server` says whether Nitro built
 * server/.
 */
export async function finalize(nitro: FinalizeNitro, { server }: { server: boolean }): Promise<FinalizeResult> {
  const o = nitro.options;
  const { dir, publicDir, serverDir } = o.output;
  const opts = o.thanksComputer ?? {};
  const warnings: string[] = [];
  const warn = (msg: string) => {
    warnings.push(msg);
    nitro.logger.warn(`[${PRESET}] ${msg}`);
  };

  // Nitro's build info: read for the manifest, then dropped (it isn't part
  // of a Web ABI build, and Nuxt 4's CLI keeps its own).
  let nitroVersion: string | undefined;
  try {
    nitroVersion = JSON.parse(await readFile(join(dir, "nitro.json"), "utf8"))?.versions?.nitro;
  } catch {}
  await rm(join(dir, "nitro.json"), { force: true });

  let entry: string | undefined;
  if (server) {
    const out = o.rollupConfig?.output as { entryFileNames?: unknown } | undefined;
    const name = typeof out?.entryFileNames === "string" ? out.entryFileNames : "index.mjs";
    const file = join(serverDir, name);
    if (!(await exists(file))) throw new Error(`[${PRESET}] the server build has no entry at ${file}`);
    entry = posix(relative(dir, file));
  } else if (await exists(serverDir)) {
    // No server was built, so server/ holds only build-time leftovers: a
    // framework can prepare it and then skip the server build, and one that
    // passes its own output paths into the config (Analog) has the
    // prerenderer bundle into it.
    await rm(serverDir, { recursive: true, force: true });
  }

  const mode: Mode = server ? "server" : isSpa(nitro) ? "spa" : "static";
  const readPage = async (name: string): Promise<Page | null> => {
    const html = await readFile(join(publicDir, name), "utf8").catch(() => null);
    return html === null ? null : { name, html };
  };
  const page = mode === "spa" ? ((await readPage("200.html")) ?? (await readPage("index.html"))) : await readPage("404.html");
  if (mode === "spa" && page === null) {
    throw new Error(`[${PRESET}] an ssr: false build needs its shell, ${join(publicDir, "200.html")} or index.html, and has neither`);
  }
  if (page && Buffer.byteLength(page.html) > PAGE_WARN_BYTES) {
    warn(`${page.name} is ${Buffer.byteLength(page.html)} bytes; the op that serves it is capped at 4 MiB, base64 included`);
  }

  const ops = renderOps({ scope: opts.scope ?? PRODUCER_SCOPE, mode, page, producer: NAME });
  await writeOps(dir, ops);

  // public/ as installed: paths relative to it (they include the app's baseURL).
  const publicRoot = join(dir, "public");
  let files = (await exists(publicRoot)) ? await listFiles(publicRoot) : [];

  // What never deploys, taken out rather than shipped: a bundler's .vite/
  // metadata (vinxi leaves one under _build/), and the .gz/.br copies
  // compressPublicAssets writes beside a file (nothing serves them; the edge
  // compresses).
  const have = new Set(files);
  const drop = files.filter(
    (f) =>
      f.split("/").includes(".vite") ||
      (Boolean(o.compressPublicAssets) && /\.(gz|br)$/.test(f) && have.has(f.replace(/\.(gz|br)$/, ""))),
  );
  for (const f of drop) await rm(join(publicRoot, f), { force: true });
  for (const d of new Set(files.filter((f) => f.split("/").includes(".vite")).map((f) => f.slice(0, f.indexOf(".vite/") + 5)))) {
    await rm(join(publicRoot, d), { recursive: true, force: true });
  }
  const compressed = drop.filter((f) => /\.(gz|br)$/.test(f)).length;
  if (compressed > 0) nitro.logger.info(`[${PRESET}] left out ${compressed} precompressed .gz/.br copies (the edge compresses)`);
  files = files.filter((f) => !drop.includes(f));

  let immutable: string[];
  if (opts.immutable) {
    const problems = checkImmutable(opts.immutable);
    if (problems.length > 0) throw new Error(`[${PRESET}] thanksComputer.immutable: ${problems.join("; ")}`);
    immutable = [...opts.immutable];
  } else {
    immutable = deriveImmutable({ assets: o.publicAssets, appBaseURL: o.baseURL, framework: o.framework.name ?? "" });
  }
  for (const prefix of immutable) {
    const under = files.filter((f) => f.startsWith(prefix));
    if (under.length === 0) {
      warn(`the immutable prefix ${prefix} matches no file in public/`);
      continue;
    }
    // Nuxt's builds/latest.json is fetched with a cache-busting query.
    const unhashed = under.filter((f) => !looksHashed(f.split("/").pop()!) && !/\/builds\/latest\.json$/.test(f));
    if (unhashed.length > 0) {
      warn(`${unhashed.length} file(s) under the immutable prefix ${prefix} carry no content hash (${unhashed.slice(0, 3).join(", ")}), so a change to them wouldn't reach a browser for a year`);
    }
  }

  const dots = files.filter((f) => f.split("/").some((s) => s.startsWith(".")));
  if (dots.length > 0) warn(`public/ has ${dots.length} dot path(s) (${dots.slice(0, 3).join(", ")}); they never deploy`);

  const manifest: Record<string, unknown> = {
    abi: 1,
    ...(entry ? { server: { entry } } : {}),
    ...(immutable.length > 0 ? { immutable } : {}),
    "x-producer": { name: NAME, version: VERSION },
    "x-nitro": {
      ...(nitroVersion ? { nitro: nitroVersion } : {}),
      preset: PRESET,
      framework: { name: o.framework.name || "nitro", ...(o.framework.version ? { version: o.framework.version } : {}) },
      mode,
    },
  };
  await writeManifest(dir, manifest);

  nitro.logger.success(`[${PRESET}] Web ABI build (${mode}) at ${dir}: ${files.length} public file(s), ${Object.keys(ops).length} op(s)${entry ? `, server entry ${entry}` : ""}`);
  nitro.logger.info(
    `[${PRESET}] Next: \`txco web check ${relative(process.cwd(), dir) || "."}${entry ? " --static-only" : ""}\`, bind it in txco.yaml (stacks: <name>: abi: <dir>), then \`txco apply\`.` +
      (entry ? " No chassis runs server/ yet: deploy the static half with --static-only." : ""),
  );
  return { mode, manifest, ops: Object.keys(ops).sort(), warnings };
}
