# @txco/astro

An [Astro](https://astro.build) integration for
[Thanks, Computer](https://www.thanks.computer) (txco). After `astro build`,
it writes a **Web ABI build** beside your `dist/`:

- a copy of the built site as `public/`;
- the 404 page and a catch-all, as `ops/`;
- a `txco-web.json` manifest.

`txco apply` installs the build into a stack.

```sh
npm install -D @txco/astro
```

Astro 5, 6 or 7, with static output.

## Usage

```js
// astro.config.mjs
import { defineConfig } from "astro/config";
import txco from "@txco/astro";

export default defineConfig({
  integrations: [txco()],
});
```

Bind the build to a stack in your workspace's `txco.yaml`. The path is
relative to the workspace root, and must lie outside `OPS/`:

```yaml
stacks:
  web:
    abi: txco-web
```

Then build, check and deploy:

```sh
astro build                   # writes dist/ as usual, and txco-web/
txco web check txco-web       # installs it on a scratch chassis and probes it
txco apply                    # or: txco push web
```

Gitignore `txco-web/`. It's all build output, and the 404 op embeds your
404 page, so it changes on every build.

## How a request is answered

Every page of an Astro site is a file, so:

- **a page** is served as a file: `/about` from `about/index.html`, or from
  `about.html` with `build.format: "file"`;
- **a path with no page** gets your `404.html` (from `src/pages/404.astro`)
  with status 404, or a plain 404 page if there's none;
- **everything else**, such as a missing asset or a POST, gets a plain 404.

Astro's hashed output under `_astro/` is cached for a year. With
`base: "/docs"` the site lands under `public/docs/`, and is served at
`/docs/`.

## Server output

With `output: "server"`, Astro builds a server, and no chassis runs one yet.
The integration deploys the prerendered pages and warns.

## Options

| Option      | Default        | Meaning |
| ----------- | -------------- | ------- |
| `out`       | `"txco-web"`   | The Web ABI directory to write, relative to the project root. It must be a directory of its own, outside `OPS/` and `dist/`. |
| `immutable` | `["_astro/"]`  | The `public/` prefixes cached for a year. |
| `scope`     | `900000`       | The 404 op's scope. The catch-all goes 900 above it. |

## License

MIT
