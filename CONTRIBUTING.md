# Contributing

Thanks for your interest in `txco`.

`txco` is open source under the [Mozilla Public License 2.0](./LICENSE) — read,
build, run, self-host, and modify it freely. Contributions are welcome; by
submitting a pull request you agree that your contribution is licensed under the
MPL-2.0. For substantial changes, please open an issue first to discuss the
approach before investing significant effort.

## Reporting issues

- **Bugs / features** — open a GitHub issue with a clear repro and your
  environment (OS, `txco version`).
- **Security vulnerabilities** — do **not** file a public issue; follow
  [SECURITY.md](./SECURITY.md).

## Building from source

Requires Go (see `go.mod` for the version) and, for the embedded web UIs,
Node + [pnpm](https://pnpm.io). From the repo root:

```sh
make build          # builds the admin + continuation UIs, then the txco binary
./chassis/bin/txco --help
```

`make build` runs the UI builds first (Vite writes the bundles into the
`//go:embed` dirs) and then compiles `./cmd/txco`. A bare `go build ./cmd/txco`
also works but ships placeholder web UIs.

## Before opening a pull request

- `cd chassis && go test ./...` is green.
- `cd admin-ui && pnpm run check && pnpm test` is green (and likewise for
  `continuation-ui` if you touched it).
- Keep changes focused; match the style and structure of surrounding code.
- Note any user-visible or config changes in the PR description.
