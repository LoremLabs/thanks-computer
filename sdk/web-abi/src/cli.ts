#!/usr/bin/env node
// txco-web-abi: validate a manifest, check a build's server/, or serve a
// build locally.

import { readFile } from "node:fs/promises";
import { join } from "node:path";

import { checkServer, serve } from "./harness.js";
import { MANIFEST_NAME, validateManifest } from "./manifest.js";

const USAGE = `Usage: txco-web-abi <command> <abi-dir>

Commands:
  validate <abi-dir>                 check txco-web.json against the Web ABI schema
  check-server <abi-dir> [--json]    drive the build's server/ entry through the bridge
  serve <abi-dir> [--port N]         serve public/ and server/ locally
`;

async function main(argv: string[]): Promise<number> {
  const [cmd, dir, ...rest] = argv;
  if (!cmd || !dir) {
    process.stderr.write(USAGE);
    return 2;
  }
  switch (cmd) {
    case "validate": {
      let doc: unknown;
      try {
        doc = JSON.parse(await readFile(join(dir, MANIFEST_NAME), "utf8"));
      } catch (e) {
        process.stderr.write(`${MANIFEST_NAME}: ${(e as Error).message}\n`);
        return 1;
      }
      const r = validateManifest(doc);
      if (r.ok) {
        process.stdout.write(`${MANIFEST_NAME}: ok\n`);
        return 0;
      }
      for (const p of r.errors) process.stdout.write(`${p.pointer || "/"}: ${p.message}\n`);
      return 1;
    }
    case "check-server": {
      const results = await checkServer(dir);
      if (rest.includes("--json")) {
        process.stdout.write(JSON.stringify({ ok: results.every((r) => r.ok), results }, null, 2) + "\n");
      } else {
        for (const r of results) process.stdout.write(`${r.ok ? "✓" : "✗"} ${r.name}${r.detail ? "  " + r.detail : ""}\n`);
      }
      return results.every((r) => r.ok) ? 0 : 1;
    }
    case "serve": {
      const i = rest.indexOf("--port");
      const port = i >= 0 ? Number(rest[i + 1]) : 8787;
      const server = await serve(dir, { port });
      const addr = server.address();
      process.stdout.write(`serving ${dir} on http://127.0.0.1:${typeof addr === "object" && addr ? addr.port : port}\n`);
      return await new Promise<number>(() => {}); // until interrupted
    }
    default:
      process.stderr.write(USAGE);
      return 2;
  }
}

main(process.argv.slice(2)).then(
  (code) => process.exit(code),
  (e) => {
    process.stderr.write(`txco-web-abi: ${(e as Error).message}\n`);
    process.exit(1);
  },
);
