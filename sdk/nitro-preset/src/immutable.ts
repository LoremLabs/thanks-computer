// Which public/ prefixes the manifest may call immutable (cached for a year).
// Pure, so it's unit-tested.

/** One of Nitro's public asset dirs, as `nitro.options.publicAssets` holds it. */
export interface AssetDir {
  baseURL?: string;
  maxAge?: number;
}

export interface ImmutableInput {
  assets: readonly AssetDir[];
  /** The app's baseURL ("/" for most apps). */
  appBaseURL: string;
  /** `nitro.options.framework.name` ("nuxt", "analog", "nitro", …). */
  framework: string;
}

/** A year: what a framework sets on its content-hashed asset dir. */
export const YEAR = 31536000;

const trim = (u: string) => u.replace(/^\/+|\/+$/g, "");
const norm = (u: string | undefined) => "/" + trim(u ?? "/");
const inside = (inner: string, outer: string) => outer === "/" || inner === outer || inner.startsWith(outer + "/");

/**
 * The prefixes (relative to public/, ending in "/") whose files the
 * framework already caches for a year: its content-hashed asset dir, like
 * Nuxt's `_nuxt/`.
 *
 * A candidate is a non-root asset dir with a year's maxAge. It is dropped
 * when a dir with a shorter maxAge shares or sits inside it, since those files
 * change between builds. The exception is Nuxt's `<buildAssetsDir>/builds`:
 * Nuxt fetches its `latest.json` with a cache-busting query. Nested
 * candidates collapse into the outer one.
 */
export function deriveImmutable(input: ImmutableInput): string[] {
  const dirs = input.assets.map((a) => ({ base: norm(a.baseURL), maxAge: a.maxAge ?? 0 }));
  const kept = dirs
    .filter((c) => c.maxAge >= YEAR && c.base !== "/")
    .filter(
      (c) =>
        !dirs.some(
          (o) =>
            o.maxAge < YEAR &&
            o.base !== "/" &&
            inside(o.base, c.base) &&
            !(input.framework === "nuxt" && o.base === c.base + "/builds"),
        ),
    )
    .map((c) => c.base);
  const outer = [...new Set(kept)].filter((b) => !kept.some((o) => o !== b && inside(b, o)));
  const app = trim(input.appBaseURL);
  return outer.map((b) => (app ? `${app}/` : "") + `${trim(b)}/`).sort();
}

/** What's wrong with an explicit `immutable` list, if anything. */
export function checkImmutable(prefixes: readonly string[]): string[] {
  const problems: string[] = [];
  for (const p of prefixes) {
    const segs = p.split("/").slice(0, -1);
    if (!p.endsWith("/") || p.startsWith("/") || segs.length === 0) {
      problems.push(`"${p}": an immutable prefix is a directory relative to public/, ending in "/" (e.g. "assets/")`);
    } else if (segs.some((s) => s === "" || s.startsWith("."))) {
      problems.push(`"${p}": no empty or dot segments`);
    } else if (segs[0] === "_txco") {
      problems.push(`"${p}": _txco/ is reserved for the installer`);
    }
  }
  return problems;
}

/** Whether a file name carries a content hash (the kit's heuristic, shared by every producer). */
export { looksHashed } from "@txco/web-abi/producer";
