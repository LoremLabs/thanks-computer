// @txco/nitro-preset: the `thanks-computer` Nitro preset. It builds a Nitro
// app (Nuxt, Analog) as a Web ABI build for `txco apply` to install:
//
//   txco-web/
//     txco-web.json   the manifest
//     public/         the site: Nitro's public output, prerendered pages included
//     ops/            the navigation op and the catch-all
//     server/         the app's server as a Fetch handler (`nuxi build` only)
//
//   // nuxt.config.ts
//   export default defineNuxtConfig({ nitro: { preset: "@txco/nitro-preset" } })
//
// Written in Nitro's in-tree preset shape, so it can move upstream. It
// doesn't import nitropack at runtime: a Nuxt app doesn't depend on it
// directly, and a strict package manager won't resolve it from here.
import { fileURLToPath } from "node:url";

import { PRESET } from "./finalize.js";
import { thanksComputerModule } from "./module.js";

export type { ThanksComputerOptions } from "./types.js";

const preset = {
  entry: fileURLToPath(new URL("./runtime/entry.js", import.meta.url)),
  // Node built-ins stay native; every dependency is bundled into server/,
  // which deploys without an install step.
  node: true,
  noExternals: true,
  // The chassis serves public/ before the app's stack runs.
  serveStatic: false,
  output: {
    dir: "{{ rootDir }}/txco-web",
    serverDir: "{{ output.dir }}/server",
    publicDir: "{{ output.dir }}/public/{{ baseURL }}",
  },
  commands: {
    deploy: "txco apply",
  },
  modules: [thanksComputerModule],
  // What defineNitroPreset (nitropack/kit) adds, inlined.
  _meta: { name: PRESET, url: import.meta.url },
};

export default preset;
