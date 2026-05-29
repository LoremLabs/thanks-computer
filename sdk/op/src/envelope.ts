// @txco/op/envelope — small helpers for reading/writing nested envelope paths.
//
//   import { get, set, pick } from "@txco/op/envelope";

type Path = string | Array<string | number>;

function toSegments(path: Path): Array<string | number> {
  if (Array.isArray(path)) return path;
  // dot path with optional [index]: "a.b[0].c"
  const out: Array<string | number> = [];
  for (const part of path.split(".")) {
    const m = part.match(/^([^[]*)((\[\d+\])*)$/);
    if (!m) {
      out.push(part);
      continue;
    }
    if (m[1] !== "") out.push(m[1]);
    const idx = m[2].match(/\d+/g);
    if (idx) for (const i of idx) out.push(Number(i));
  }
  return out;
}

/** Read a nested value by dot/array path; returns `dflt` if any segment is missing. */
export function get<T = unknown>(obj: unknown, path: Path, dflt?: T): T | undefined {
  let cur: any = obj;
  for (const seg of toSegments(path)) {
    if (cur == null) return dflt;
    cur = cur[seg as any];
  }
  return cur === undefined ? dflt : cur;
}

/** Set a nested value by dot/array path, creating intermediate objects/arrays.
 *  Mutates and returns `obj`. */
export function set<T extends object>(obj: T, path: Path, value: unknown): T {
  const segs = toSegments(path);
  let cur: any = obj;
  for (let i = 0; i < segs.length - 1; i++) {
    const seg = segs[i];
    const next = segs[i + 1];
    if (cur[seg] == null || typeof cur[seg] !== "object") {
      cur[seg] = typeof next === "number" ? [] : {};
    }
    cur = cur[seg];
  }
  cur[segs[segs.length - 1] as any] = value;
  return obj;
}

/** Shallow-copy only the named keys from `obj`. */
export function pick<T extends object, K extends keyof T>(obj: T, keys: K[]): Pick<T, K> {
  const out = {} as Pick<T, K>;
  for (const k of keys) {
    if (k in obj) out[k] = obj[k];
  }
  return out;
}
