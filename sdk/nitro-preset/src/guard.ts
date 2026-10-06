// The check that runs before Nitro wipes the output dir. Pure (paths in,
// problems out), so it's unit-tested; the module reads the dir's entries.
// The generic checks are the kit's; the layout check is Nitro's.
import { join, resolve } from "node:path";

import { checkOutDir as checkABIOutDir } from "@txco/web-abi/producer";

export interface OutDirInput {
  /** `nitro.options.output.dir`. */
  dir: string;
  /** `nitro.options.output.publicDir`. */
  publicDir: string;
  /** `nitro.options.output.serverDir`. */
  serverDir: string;
  /** The app's baseURL ("/" for most apps). */
  baseURL: string;
  /** Dirs the wipe must not touch: rootDir, srcDir, buildDir, the public asset sources. */
  protect: readonly string[];
  /** The dir's current entries, or null when it doesn't exist. */
  entries: readonly string[] | null;
}

/** Every reason the build mustn't write (and so wipe) the output dir. Empty when it may. */
export function checkOutDir(i: OutDirInput): string[] {
  const dir = resolve(i.dir);
  const problems: string[] = [];
  const base = i.baseURL.replace(/^\/+|\/+$/g, "");
  const wantPublic = resolve(join(dir, "public", base));
  const wantServer = resolve(join(dir, "server"));
  if (resolve(i.publicDir) !== wantPublic || resolve(i.serverDir) !== wantServer) {
    problems.push(
      `the output dirs don't follow the Web ABI layout: publicDir is ${resolve(i.publicDir)} (want ${wantPublic}) and serverDir is ${resolve(i.serverDir)} (want ${wantServer}). ` +
        `If your framework sets its own output paths (Analog does), set nitro.output.dir and nitro.output.publicDir together, or leave all three to the preset.`,
    );
  }
  // Nitro's build info is a previous build's own file.
  return [...checkABIOutDir({ dir, protect: i.protect, entries: i.entries, allow: ["nitro.json"] }), ...problems];
}
