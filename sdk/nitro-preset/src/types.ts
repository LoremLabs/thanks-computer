/** Options, set as `nitro: { thanksComputer: { … } }` (Nuxt) or the Nitro config's `thanksComputer`. */
export interface ThanksComputerOptions {
  /**
   * Answer every unknown page path with the app shell and 200, for an app
   * that routes in the browser. Detected for Nuxt `ssr: false` and Analog's
   * client renderer; set it to force the choice either way.
   */
  spa?: boolean;
  /**
   * The public/ prefixes cached for a year (`["assets/"]`), each a directory
   * ending in "/". Derived from the framework's asset dirs when unset (Nuxt:
   * `_nuxt/`).
   */
  immutable?: string[];
  /** The scope of the navigation op; the catch-all goes 900 above it. Default 900000. */
  scope?: number;
}

declare module "nitropack/presets" {
  interface PresetOptions {
    thanksComputer?: ThanksComputerOptions;
  }
}
