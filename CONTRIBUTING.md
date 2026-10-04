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
make build          # builds the admin, continuation and printer UIs, then the txco binary
./chassis/bin/txco --help
```

`make build` runs the UI builds first (Vite writes the bundles into the
`//go:embed` dirs) and then compiles `./cmd/txco`. A bare
`go build -tags sqlite_fts5 ./cmd/txco` also works but ships placeholder web UIs.
Every build and test needs `-tags sqlite_fts5` (`txco://dataset` queries use
SQLite's full-text extension); the Makefile passes it for you.

## Before opening a pull request

- `go test -tags sqlite_fts5 ./...` from the repo root is green (`make qtest`
  runs the chassis packages with `-race`).
- `cd admin-ui && pnpm run check && pnpm test` is green, and `pnpm run check`
  in `continuation-ui` or `printer-ui` if you touched them.
- Keep changes focused; match the style and structure of surrounding code.
- Note any user-visible or config changes in the PR description.
