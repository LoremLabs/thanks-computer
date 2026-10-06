// The Web ABI manifest (txco-web.json): what a build is, never its routing.
// This validator mirrors txco-web.schema.json plus the rules a schema can't
// express, and is held to the same fixture corpus as the Go validator
// (chassis/webabi), so the two agree.

import { readFile } from "node:fs/promises";
import { join } from "node:path";

/** The manifest's file name at the root of an ABI directory. */
export const MANIFEST_NAME = "txco-web.json";
/** The manifest's abi value this kit reads. */
export const ABI_VERSION = 1;
/** Producers' ops live in the band starting here, after every author op. */
export const PRODUCER_SCOPE = 900000;

export interface Manifest {
  abi: 1;
  server?: { entry: string };
  immutable?: string[];
  [extension: `x-${string}`]: unknown;
}

export interface Problem {
  /** A JSON pointer into the manifest ("" is the root). */
  pointer: string;
  message: string;
}

export type ValidationResult = { ok: true; manifest: Manifest } | { ok: false; errors: Problem[] };

const ENTRY_RE = /^server\/[^/\\][^\\]*\.m?js$/;
const PREFIX_RE = /^[^/\\][^\\]*\/$/;

function cleanPath(p: string): boolean {
  if (p === "" || p.startsWith("/") || p.endsWith("/")) return false;
  return p.split("/").every((seg) => seg !== "" && !seg.startsWith("."));
}

/** Validates a parsed manifest. */
export function validateManifest(doc: unknown): ValidationResult {
  const errors: Problem[] = [];
  if (typeof doc !== "object" || doc === null || Array.isArray(doc)) {
    return { ok: false, errors: [{ pointer: "", message: "the manifest must be a JSON object" }] };
  }
  const m = doc as Record<string, unknown>;
  for (const key of Object.keys(m)) {
    if (!["$schema", "abi", "server", "immutable"].includes(key) && !key.startsWith("x-")) {
      errors.push({ pointer: "", message: `unknown property "${key}" (a manifest describes the build, never its routing; extensions start with x-)` });
    }
  }
  if (!("abi" in m)) {
    errors.push({ pointer: "", message: 'missing required property "abi"' });
  } else if (m.abi !== ABI_VERSION) {
    errors.push({ pointer: "/abi", message: `abi must be ${ABI_VERSION}` });
  }
  if ("$schema" in m && typeof m.$schema !== "string") {
    errors.push({ pointer: "/$schema", message: "must be a string" });
  }
  if ("server" in m) {
    const s = m.server;
    if (typeof s !== "object" || s === null || Array.isArray(s)) {
      errors.push({ pointer: "/server", message: "must be an object" });
    } else {
      const so = s as Record<string, unknown>;
      for (const key of Object.keys(so)) {
        if (key !== "entry") errors.push({ pointer: "/server", message: `unknown property "${key}"` });
      }
      if (typeof so.entry !== "string") {
        errors.push({ pointer: "/server", message: 'missing required property "entry"' });
      } else if (!ENTRY_RE.test(so.entry) || so.entry.length > 512) {
        errors.push({ pointer: "/server/entry", message: "must be a .js or .mjs module under server/" });
      } else if (!cleanPath(so.entry)) {
        errors.push({ pointer: "/server/entry", message: "the entry must be a clean path under server/" });
      }
    }
  }
  if ("immutable" in m) {
    const im = m.immutable;
    if (!Array.isArray(im)) {
      errors.push({ pointer: "/immutable", message: "must be an array of prefixes" });
    } else {
      if (new Set(im).size !== im.length) {
        errors.push({ pointer: "/immutable", message: "items must be unique" });
      }
      im.forEach((p, i) => {
        const ptr = `/immutable/${i}`;
        if (typeof p !== "string" || p.length < 2 || p.length > 512 || !PREFIX_RE.test(p)) {
          errors.push({ pointer: ptr, message: "a prefix is a relative path ending in '/'" });
        } else if (!cleanPath(p.replace(/\/$/, ""))) {
          errors.push({ pointer: ptr, message: "a prefix must be a clean path, with no '.' or '..' segment" });
        } else if (p.split("/")[0] === "_txco") {
          errors.push({ pointer: ptr, message: "_txco/ is reserved for the installer" });
        }
      });
    }
  }
  return errors.length > 0 ? { ok: false, errors } : { ok: true, manifest: m as unknown as Manifest };
}

/** Reads and validates <dir>/txco-web.json. Throws on an unreadable or invalid manifest. */
export async function readManifest(dir: string): Promise<Manifest> {
  const raw = await readFile(join(dir, MANIFEST_NAME), "utf8");
  let doc: unknown;
  try {
    doc = JSON.parse(raw);
  } catch (e) {
    throw new Error(`${MANIFEST_NAME}: not valid JSON: ${(e as Error).message}`);
  }
  const r = validateManifest(doc);
  if (!r.ok) {
    throw new Error(`${MANIFEST_NAME}: ` + r.errors.map((p) => `${p.pointer || "/"}: ${p.message}`).join("; "));
  }
  return r.manifest;
}

/**
 * The private root of a public/ path: cut at the end of its first "_"
 * segment ("" when it has none). The installer writes one public marker per
 * root, so `_app/x.js` → `_app`, `assets/_Dk3.js` → itself.
 */
export function publicRoot(rel: string): string {
  const segs = rel.split("/");
  for (let i = 0; i < segs.length; i++) {
    if (segs[i].startsWith("_")) return segs.slice(0, i + 1).join("/");
  }
  return "";
}
