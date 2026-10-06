// Writes the Web ABI build from React Router's finished output. Called from
// the preset's buildEnd; takes plain inputs, so it's tested without a build.
import { readFile, rm, stat } from "node:fs/promises";
import { join, relative, resolve } from "node:path";

import {
  copyPublic,
  listFiles,
  looksHashed,
  publicPrefix,
  renderOps,
  writeManifest,
  writeOps,
  type NavMode,
} from "@txco/web-abi/producer";

import { routesMatcher, type RouteManifest } from "./routes.js";

export const NAME = "@txco/react-router";
export const VERSION = "0.1.0";
/** An op answer is capped at 4 MiB (--op-payload-max), base64 included. */
const PAGE_WARN_BYTES = 1 << 20;

export interface TxcoOptions {
  /** The Web ABI directory to write, relative to the project root. Default "txco-web". */
  out?: string;
  /**
   * Give an unknown page path the shell with 404, from the route table.
   * false answers every page path with the shell and 200. Default true.
   */
  routeAware?: boolean;
  /** The public/ prefixes cached for a year. Default: Vite's assetsDir (`assets/`). */
  immutable?: string[];
  /** The navigation op's scope; the catch-all goes 900 above it. Default 900000. */
  scope?: number;
}

export interface BuildInput {
  /** The project root (Vite's root). */
  root: string;
  /** reactRouterConfig.buildDirectory: client output is <it>/client. */
  buildDirectory: string;
  /** Vite's base (where the assets are served). */
  base: string;
  /** Vite's build.assetsDir. */
  assetsDir: string;
  /** reactRouterConfig.basename (where the routes are). */
  basename: string;
  /** reactRouterConfig.ssr. */
  ssr: boolean;
  routes: RouteManifest;
  options: TxcoOptions;
  log: { info(msg: string): void; warn(msg: string): void };
}

export interface BuildResult {
  mode: NavMode;
  ops: string[];
  manifest: Record<string, unknown>;
}

const exists = (p: string) => stat(p).then(() => true, () => false);

export async function writeBuild(i: BuildInput): Promise<BuildResult> {
  const warn = (msg: string) => i.log.warn(`[txco] ${msg}`);
  const clientDir = resolve(i.root, i.buildDirectory, "client");
  if (!(await exists(clientDir))) throw new Error(`[txco] React Router's client build isn't at ${clientDir}`);
  const out = resolve(i.root, i.options.out ?? "txco-web");
  const { prefix, external } = publicPrefix(i.base);
  if (external) warn(`base is ${i.base}: the assets load from there, and the pages are deployed at the root`);

  await rm(out, { recursive: true, force: true });
  const { skipped } = await copyPublic(clientDir, out, prefix);
  if (skipped.length > 0) warn(`build/client has ${skipped.length} dot path(s) (${skipped.slice(0, 3).join(", ")}); they never deploy`);

  const readPage = async (name: string) => {
    const html = await readFile(join(clientDir, name), "utf8").catch(() => null);
    return html === null ? null : { name, html };
  };

  let mode: NavMode;
  let page = null;
  let routes: string | undefined;
  const notes: string[] = [];
  if (i.ssr) {
    // No chassis runs a server yet: deploy what was prerendered.
    warn("ssr: true needs a server, and no chassis runs one yet: only the prerendered pages deploy. For an SPA, set ssr: false.");
    mode = "404";
    notes.push("A server-rendered React Router app: only its prerendered pages deploy until a runner exists.");
  } else {
    // In SPA mode the shell is __spa-fallback.html when "/" is prerendered.
    page = (await readPage("__spa-fallback.html")) ?? (await readPage("index.html"));
    if (!page) throw new Error(`[txco] an ssr: false build needs its shell (index.html or __spa-fallback.html) in ${clientDir}`);
    const matcher = i.options.routeAware === false ? null : routesMatcher(i.routes, i.basename);
    if (matcher) {
      mode = "routes";
      routes = matcher;
      notes.push("The route set comes from React Router's route table.");
    } else {
      mode = "spa";
      if (i.options.routeAware !== false) notes.push("A route couldn't be expressed as a matcher, so unknown pages get 200 too.");
    }
  }
  if (page && Buffer.byteLength(page.html) > PAGE_WARN_BYTES) {
    warn(`${page.name} is ${Buffer.byteLength(page.html)} bytes; the op that serves it is capped at 4 MiB, base64 included`);
  }

  const ops = renderOps({ mode, scope: i.options.scope, producer: NAME, page, routes, notes });
  await writeOps(out, ops);

  const files = await listFiles(join(out, "public"));
  const assets = i.assetsDir.replace(/^\/+|\/+$/g, "");
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
    "x-react-router": { ssr: i.ssr, mode, routes: Object.keys(i.routes).length },
  };
  await writeManifest(out, manifest);

  const shown = relative(process.cwd(), out) || ".";
  i.log.info(
    `[txco] Web ABI build (${mode}) at ${shown}: ${files.length} public file(s), ${Object.keys(ops).length} op(s). ` +
      `Next: \`txco web check ${shown}\`, bind it in txco.yaml (stacks: <name>: abi: ${shown}), then \`txco apply\`.`,
  );
  return { mode, ops: Object.keys(ops).sort(), manifest };
}
