// React Router's route table as one RE2 matcher, for the route-aware SPA
// fallback: a navigation to a path a route knows gets the shell with 200,
// any other the shell with 404. Pure, so it's unit-tested.

/** One entry of React Router's flat RouteManifest. */
export interface RouteEntry {
  id: string;
  parentId?: string;
  path?: string;
  index?: boolean;
  caseSensitive?: boolean;
}

export type RouteManifest = Record<string, RouteEntry>;

/** Every route's full path pattern (joined up its parentId chain), deduped. */
export function routePaths(routes: RouteManifest): { path: string; caseSensitive: boolean }[] {
  const seen = new Map<string, boolean>();
  for (const r of Object.values(routes)) {
    const parts: string[] = [];
    let caseSensitive = false;
    for (let cur: RouteEntry | undefined = r, n = 0; cur && n < 100; cur = cur.parentId ? routes[cur.parentId] : undefined, n++) {
      if (cur.path) parts.unshift(cur.path.replace(/^\/+|\/+$/g, ""));
      if (cur.caseSensitive) caseSensitive = true;
    }
    const path = parts.filter(Boolean).join("/");
    seen.set(path, (seen.get(path) ?? false) || caseSensitive);
  }
  return [...seen].map(([path, caseSensitive]) => ({ path, caseSensitive })).sort((a, b) => a.path.localeCompare(b.path));
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/**
 * One route path as a regex source matching a request path (leading "/",
 * an optional trailing "/"). React Router's syntax: `:param`, `:param?`,
 * a static `segment?`, and a final `*`. null for anything else.
 */
export function routeRegex(path: string, caseSensitive = false): string | null {
  const segs = path.split("/").filter(Boolean);
  let re = "";
  for (let i = 0; i < segs.length; i++) {
    const seg = segs[i];
    if (seg === "*") {
      if (i !== segs.length - 1) return null;
      re += "(?:/.*)?";
    } else if (/^:[A-Za-z0-9_-]+\?$/.test(seg)) {
      re += "(?:/[^/]+)?";
    } else if (/^:[A-Za-z0-9_-]+$/.test(seg)) {
      re += "/[^/]+";
    } else if (seg.includes(":") || seg.includes("*")) {
      return null; // a param or splat inside a segment: not React Router syntax
    } else if (seg.endsWith("?")) {
      re += `(?:/${escapeRe(seg.slice(0, -1))})?`;
    } else {
      re += `/${escapeRe(seg)}`;
    }
  }
  re += "/?";
  return caseSensitive ? re : `(?i:${re})`;
}

/**
 * The anchored matcher for every route, under `basename`, with "/" escaped
 * for txcl's /…/ literal. null when a route can't be translated (the caller
 * then answers every navigation with the shell and 200).
 */
export function routesMatcher(routes: RouteManifest, basename = "/"): string | null {
  // React Router matches basename case-insensitively (stripBasename).
  const base = basename.replace(/^\/+|\/+$/g, "");
  const prefix = base ? `(?i:/${escapeRe(base)})` : "";
  const alts: string[] = [];
  for (const { path, caseSensitive } of routePaths(routes)) {
    const re = routeRegex(path, caseSensitive);
    if (re === null) return null;
    alts.push(prefix + re);
  }
  if (alts.length === 0) return null;
  return `^(?:${alts.join("|")})$`.replace(/\//g, "\\/");
}
