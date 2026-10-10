# @txco/react-router

A [React Router](https://reactrouter.com) preset for
[Thanks, Computer](https://www.thanks.computer) (txco). After
`react-router build`, it writes a **Web ABI build** beside your `build/`:

- a copy of `build/client` as `public/`;
- a route-aware SPA fallback and a catch-all, as `ops/`;
- a `txco-web.json` manifest.

`txco apply` installs the build into a stack.

```sh
npm install -D @txco/react-router
```

React Router 7 and 8, in framework mode.

## Usage

```ts
// react-router.config.ts
import type { Config } from "@react-router/dev/config";
import txco from "@txco/react-router";

export default {
  ssr: false, // SPA mode: the client renders every page
  prerender: ["/about"], // optional: pages served as files
  presets: [txco()],
} satisfies Config;
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
react-router build            # writes build/ as usual, and txco-web/
txco web check txco-web       # installs it on a scratch chassis and probes it
txco apply                    # or: txco push web
```

Gitignore `txco-web/`. It's all build output, and the fallback ops embed the
app shell, asset hashes included, so they change on every build.

## How a request is answered

1. **A file**, such as a prerendered page (`/about` → `about/index.html`), an
   asset, or a `.data` file, is served before your stack runs.
2. **Your own ops** in the stack run next.
3. **The preset's ops** go last:
   - a GET or HEAD of a path your routes know (`/users/42` for
     `users/:id`, and `/users/4.2` too: a param may hold a dot) gets the app
     shell with status 200;
   - any other navigation gets the shell with status 404, and the client
     renders its error boundary;
   - everything else, such as a missing asset or a POST, gets a plain 404.

The matcher comes from React Router's own route table: params (`:id`),
optional segments (`:lang?`, `beta?`), a final splat (`*`) and `basename`.
It's case-insensitive unless a route sets `caseSensitive`. A splat route
makes every path known. A route the matcher can't express makes every
navigation get the shell with 200.

When `/` is prerendered, React Router writes the shell as
`__spa-fallback.html`, and the preset uses that.

## Server rendering

With `ssr: true`, React Router builds a server, and no chassis runs one yet.
The preset deploys what was prerendered, gives every other page the 404 page,
and warns. For an app the client renders, set `ssr: false`.

## Options

| Option       | Default        | Meaning |
| ------------ | -------------- | ------- |
| `out`        | `"txco-web"`   | The Web ABI directory to write, relative to the project root. It must be a directory of its own, outside `OPS/` and `build/`. |
| `routeAware` | `true`         | `false` gives every page path the shell with 200. |
| `immutable`  | `["assets/"]`  | The `public/` prefixes cached for a year. |
| `scope`      | `900000`       | The navigation ops' scope. The catch-all goes 900 above it. |

## License

MIT
