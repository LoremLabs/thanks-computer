// What every Web ABI producer writes beside public/: the ops that answer a
// request no file did, the manifest, and the guard that runs before a build
// overwrites its output dir. The producers (a framework adapter, a Nitro
// preset, a Vite plugin) share it, so their ops stay the same rules
// (chassis/webabi checks that they agree).
import { cp, mkdir, readdir, rm, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";

import { MANIFEST_NAME, PRODUCER_SCOPE, validateManifest } from "./manifest.js";

/** Where the catch-all sits: after the navigation op, since ops in one scope run concurrently. */
export const catchAllScope = (scope: number): number => scope + 900;

/**
 * A navigation: an HTTP GET or HEAD of a path whose last segment has no
 * extension. Everything else (an asset miss, a POST) is the catch-all's.
 */
export const NAV_GUARD =
  '@src == "http" && (@web.req.method == "GET" || @web.req.method == "HEAD")\n' +
  "     && @web.req.url.path !~ /(?i)\\.[a-z0-9]+$/";

/** The page a 404 op serves when the build has no 404.html. */
export const BUILTIN_404 =
  '<!doctype html>\n<html lang="en"><head><meta charset="utf-8"><title>404 Not Found</title></head>' +
  "<body><h1>404 Not Found</h1></body></html>\n";

/** An HTML page from public/, embedded in an op. */
export interface Page {
  /** Its file name in public/, for the comments. */
  name: string;
  html: string;
}

/**
 * How a page navigation nothing else answered is handled:
 *
 *  - "spa": the app routes in the browser: 200, with the app shell;
 *  - "routes": the same, but only for a path the framework's route table
 *    knows (`routes`); any other navigation gets the shell with 404;
 *  - "404": every page is a file in public/, so a path that reached the ops
 *    has no page: 404, with the 404 page;
 *  - "none": no navigation op, only the catch-all.
 */
export type NavMode = "spa" | "routes" | "404" | "none";

export interface RenderOptions {
  mode: NavMode;
  /** The navigation op's scope; the catch-all goes 900 above it. Default 900000. */
  scope?: number;
  /** The generated-file header's producer name. */
  producer: string;
  /**
   * The shell ("spa" and "routes": required), the 404 page ("404": null
   * writes a built-in one), or null ("none").
   */
  page: Page | null;
  /**
   * For "routes": an RE2 regex source, anchored, matching exactly the app's
   * page paths, with "/" escaped for txcl's /…/ literal.
   */
  routes?: string;
  /** The producer's own comment lines, added to the navigation op's header. */
  notes?: readonly string[];
}

/**
 * Renders the build's ops, as path (relative to ops/) → file text: one
 * navigation op for the mode, and the catch-all, a plain 404 for every other
 * HTTP request, so every request reaching the end of the stack is answered.
 */
export function renderOps(o: RenderOptions): Record<string, string> {
  const scope = o.scope ?? PRODUCER_SCOPE;
  const notes = (o.notes ?? []).map((l) => (l ? `# ${l}` : "#")).join("\n");
  const extra = notes ? `${notes}\n` : "";
  const out: Record<string, string> = {};
  if (o.mode === "routes") {
    if (o.page === null) throw new Error("renderOps: a routes build needs its shell");
    if (!o.routes) throw new Error("renderOps: a routes build needs its route matcher");
    out[`${scope}/spa-fallback.txcl`] = `# ${o.producer} — SPA fallback: known routes (generated; do not edit by hand).
#
# Serves the app shell (${o.page.name}) with 200 for a navigation to a known
# page route that no file or earlier op answered. Paired with spa-404.txcl.
${extra}# Regenerated on every build — the shell embeds content-hashed asset URLs.
WHEN ${NAV_GUARD}
     && @web.req.url.path =~ /${o.routes}/
${emitPage(200, o.page.html)}`;
    out[`${scope}/spa-404.txcl`] = `# ${o.producer} — SPA 404 (generated; do not edit by hand).
#
# A navigation matching no known page route is a real miss: 404, with the
# shell as the body so the client renders its error page.
WHEN ${NAV_GUARD}
     && @web.req.url.path !~ /${o.routes}/
${emitPage(404, o.page.html)}`;
  } else if (o.mode === "spa") {
    if (o.page === null) throw new Error("renderOps: an spa build needs its shell");
    out[`${scope}/spa-fallback.txcl`] = `# ${o.producer} — SPA fallback (generated; do not edit by hand).
#
# Serves the app shell (${o.page.name}) with 200 for any page navigation
# nothing else answered: the app routes in the browser, so unknown pages get
# 200 too and the client renders its not-found view.
${extra}# Regenerated on every build — the shell embeds content-hashed asset URLs.
WHEN ${NAV_GUARD}
${emitPage(200, o.page.html)}`;
  } else if (o.mode === "404") {
    const what = o.page
      ? `Serves ${o.page.name} with 404`
      : "Serves a plain 404 page (the build has no 404.html)";
    out[`${scope}/page-404.txcl`] = `# ${o.producer} — 404 page (generated; do not edit by hand).
#
# ${what} for any page navigation nothing else answered:
# no file in public/ and no op of the stack had a page for it.
${extra}# Regenerated on every build — the page embeds content-hashed asset URLs.
WHEN ${NAV_GUARD}
${emitPage(404, o.page ? o.page.html : BUILTIN_404)}`;
  }
  out[`${catchAllScope(scope)}/not-found.txcl`] = `# ${o.producer} — not found (generated; do not edit by hand).
#
# Every other HTTP request that reached the end of the stack: an asset that
# doesn't exist, a method nothing handles. Without it the request would get
# the envelope back as JSON. It sits after the navigation ops on purpose: ops
# in one scope run concurrently.
WHEN @src == "http"
  EMIT @web.res.status = 404,
       @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
       @web.res.body = b64"404 not found\\n",
       @halt = true
`;
  return out;
}

/** The EMIT tail for a page answer: the page as a readable b64"…" literal, and halt. */
export function emitPage(status: number, html: string): string {
  // The lexer base64-encodes a b64"…" literal at parse time, so the file stays
  // readable HTML. Only backslash and double-quote need escaping.
  const escaped = html.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  return `  EMIT @web.res.status = ${status},
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.headers.cache-control.0 = "no-cache",
       @web.res.body = b64"${escaped}",
       @halt = true
`;
}

/** What may already be in an output dir: a previous build, and nothing else. */
export const ABI_ENTRIES: ReadonlySet<string> = new Set([MANIFEST_NAME, "public", "server", "ops", ".gitignore", ".DS_Store"]);

export interface OutDirCheck {
  /** The output dir the build is about to wipe and rewrite. */
  dir: string;
  /** Paths the wipe must not reach: the project root, the sources, another build's output. */
  protect?: readonly string[];
  /** Paths the output dir must not sit inside (a bundler's own outDir, which it empties). */
  notInside?: readonly string[];
  /** The dir's current entries, or null when it doesn't exist. */
  entries: readonly string[] | null;
  /** Entries a producer's own previous build leaves, besides ABI_ENTRIES. */
  allow?: readonly string[];
}

const within = (child: string, parent: string) => {
  const rel = relative(parent, child);
  return rel === "" || (!rel.startsWith("..") && !isAbsolute(rel));
};

/** Every reason a build mustn't wipe and rewrite the output dir. Empty when it may. */
export function checkOutDir(c: OutDirCheck): string[] {
  const dir = resolve(c.dir);
  const problems: string[] = [];
  if (dir.split(sep).includes("OPS")) {
    problems.push(
      `the output dir ${dir} is inside OPS/. A Web ABI build lives outside the stack tree; txco.yaml binds it to a stack (stacks: <name>: abi: <dir>).`,
    );
  }
  for (const p of c.protect ?? []) {
    if (p && within(resolve(p), dir)) {
      problems.push(`the output dir ${dir} holds ${resolve(p)}, and the build wipes the output dir. Point it at a directory of its own.`);
    }
  }
  for (const p of c.notInside ?? []) {
    if (p && within(dir, resolve(p))) {
      problems.push(`the output dir ${dir} is inside ${resolve(p)}, another build's output. Point it at a directory of its own.`);
    }
  }
  const allowed = new Set([...ABI_ENTRIES, ...(c.allow ?? [])]);
  const foreign = (c.entries ?? []).filter((e) => !allowed.has(e));
  if (foreign.length > 0) {
    problems.push(
      `the output dir ${dir} holds ${foreign.map((e) => `"${e}"`).join(", ")}, which isn't part of a Web ABI build, and the build wipes it. Point the output somewhere else, or empty it.`,
    );
  }
  return problems;
}

/** Replaces <out>/ops/ with the given ops (path relative to ops/ → text). */
export async function writeOps(out: string, ops: Record<string, string>): Promise<void> {
  await rm(join(out, "ops"), { recursive: true, force: true });
  for (const [rel, text] of Object.entries(ops)) {
    const file = join(out, "ops", rel);
    await mkdir(dirname(file), { recursive: true });
    await writeFile(file, text);
  }
}

/** Validates the manifest, then writes <out>/txco-web.json. Throws on an invalid one. */
export async function writeManifest(out: string, manifest: Record<string, unknown>): Promise<void> {
  const r = validateManifest(manifest);
  if (!r.ok) {
    throw new Error(`${MANIFEST_NAME}: ` + r.errors.map((p) => `${p.pointer || "/"}: ${p.message}`).join("; "));
  }
  await mkdir(out, { recursive: true });
  await writeFile(join(out, MANIFEST_NAME), JSON.stringify(manifest, null, 2) + "\n");
}

/** Every file under root, as "/"-separated paths relative to it, sorted. */
export async function listFiles(root: string): Promise<string[]> {
  const out: string[] = [];
  for (const e of await readdir(root, { recursive: true, withFileTypes: true })) {
    // parentPath is Node ≥ 20.12; earlier 20.x calls it path.
    const parent = (e as { parentPath?: string; path?: string }).parentPath ?? (e as { path?: string }).path ?? root;
    if (e.isFile()) out.push(relative(root, join(parent, e.name)).split(sep).join("/"));
  }
  return out.sort();
}

/**
 * Whether a file name carries a content hash: Vite's `main-BxYz12Ab.js`,
 * Nuxt's `entry.CdEf34Gh.css`. A heuristic, for warnings only.
 */
export function looksHashed(name: string): boolean {
  const stem = name.replace(/\.[^.]+$/, "");
  const run = stem.match(/[A-Za-z0-9_-]{8,}/);
  return run !== null && /[0-9A-Z]/.test(run[0]);
}

/**
 * Where a build lands under public/, from its base path: "/app/" serves the
 * site under /app/, so its files go under public/app/. A relative or root
 * base serves it at the root. A full URL (a CDN) loads the assets from
 * elsewhere: the files still go to the root, and the producer should warn.
 */
export function publicPrefix(base: string): { prefix: string; external: boolean } {
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(base) || base.startsWith("//")) return { prefix: "", external: true };
  const path = base.replace(/^\.?\/+|\/+$/g, "");
  return { prefix: path && path !== "." ? `${path}/` : "", external: false };
}

/** Whether a "/"-separated relative path is one a Web ABI build never deploys: a dot segment. */
export function isDotPath(rel: string): boolean {
  return rel.split("/").some((s) => s.startsWith("."));
}

/**
 * Copies a framework's static output dir into <out>/public/<prefix>,
 * leaving out every dot path (they never deploy). Returns the ones it left
 * out, other than .vite/ (build metadata), for the producer to warn about.
 */
export async function copyPublic(from: string, out: string, prefix = ""): Promise<{ skipped: string[] }> {
  const skipped: string[] = [];
  await cp(from, join(out, "public", prefix), {
    recursive: true,
    filter: (src) => {
      const rel = relative(from, src).split(sep).join("/");
      if (rel === "" || !isDotPath(rel)) return true;
      if (rel !== ".vite" && !rel.startsWith(".vite/")) skipped.push(rel);
      return false;
    },
  });
  return { skipped };
}
