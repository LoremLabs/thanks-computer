// The plugin's decisions about a finished build. Pure, so they're unit-tested.

/**
 * How a page navigation nothing else answered is handled:
 *
 *  - "spa": one page (index.html), which routes in the browser: every page
 *    path gets the shell with 200;
 *  - "mpa": several pages, each a file: a path with no page gets 404.html
 *    (or a built-in 404 page) with 404.
 */
export type Mode = "spa" | "mpa";

/** The mode for a build's HTML pages, unless the `spa` option forces it. */
export function chooseMode(pages: readonly string[], spa?: boolean): Mode {
  if (typeof spa === "boolean") return spa ? "spa" : "mpa";
  return pages.length > 1 ? "mpa" : "spa";
}

/** The page an spa build's fallback serves: index.html, or its only page. */
export function shellPage(pages: readonly string[]): string | null {
  if (pages.includes("index.html")) return "index.html";
  return pages.length === 1 ? pages[0] : null;
}

// Where the build lands under public/, from `base`, and which paths never
// deploy: shared with every producer.
export { isDotPath, publicPrefix } from "@txco/web-abi/producer";
