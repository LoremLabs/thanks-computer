# Developing the txcl VS Code extension

## Iterate

```sh
code chassis/txcl/vscode
# F5 → child VS Code window with the grammar loaded.
# After edits: Cmd+Shift+P → "Developer: Reload Window".
```

In the child window, use **Developer: Inspect Editor Tokens and Scopes**
to see which scope a token resolves to. Exercise the grammar against
`chassis/examples/**/*.txcl`.

## Package + install locally

```sh
cd chassis/txcl/vscode
npx @vscode/vsce package                            # → txcl-<version>.vsix
code --install-extension txcl-<version>.vsix --force
```

`--force` overwrites a same-version install so you don't have to bump
the version on every iteration.

## Publish to the VS Code Marketplace

One-time setup:

1. Free Azure DevOps org at https://dev.azure.com.
2. Personal Access Token — User Settings → PAT → New Token, scope
   **Marketplace → Manage**, organization **All accessible**. Save it
   somewhere safe; it's shown only once.
3. Register the `loremlabs` publisher at
   https://marketplace.visualstudio.com/manage/createpublisher.

Each release, after bumping `package.json` by hand and writing the
CHANGELOG entry:

```sh
npx @vscode/vsce package                            # → txcl-<version>.vsix
npx @vscode/vsce publish --packagePath txcl-<version>.vsix --pat <token>
```

Publish the packaged file, not `vsce publish minor`: that bumps the
version a second time, and `vsce publish` without `--packagePath` may
also create a git commit and a `v<version>` tag — the repo's
`release.yml` cuts a txco release on any pushed `v*` tag. If the
authenticated CLI call hangs, upload the `.vsix` on the publisher's
manage page instead (⋯ → Update); that path needs no token.

Live at `https://marketplace.visualstudio.com/items?itemName=loremlabs.txcl`
within a few minutes.

## Publish to Open VSX (Cursor, VSCodium, code-server, etc.)

Same `.vsix`, different registry — covers editors that can't hit the
Microsoft Marketplace.

```sh
npx ovsx create-namespace loremlabs -p <token>      # one-time
npx ovsx publish txcl-<version>.vsix -p <token>     # each release
```

Account at https://open-vsx.org via GitHub OAuth.

## Keep in sync with the canonical grammar

The TextMate grammar at `syntaxes/txcl.tmLanguage.json` mirrors the Go
lexer at `chassis/txcl/lexer/lexer.go`, the token table at
`chassis/txcl/token/token.go`, and the parser's contextual words
(`EVERY`, `MAX`, `in`, `exists`). When those change, update in lockstep:

1. This grammar (and the snippets, README, CHANGELOG, version).
2. The TS port at `admin-ui/src/lib/txcl/lexer.ts` + `tokens.ts`, which
   has a Go-output parity test in `lexer.test.ts`; regenerate its
   fixtures with `go test ./chassis/txcl/lexer/... -run TestGenerateFixtures -update`.
3. The admin UI's CodeMirror tokenizer `admin-ui/src/lib/txcl/codemirror.ts`
   (+ `theme.ts` for a new tag) and the help panel
   `admin-ui/src/components/TxclHelp.svelte`.
4. The onepony Lab's copy of that tokenizer, `onepony/lab/src/core/txcl/highlight.ts`
   + `tokens.ts` (byte-identical to the admin UI's), colours in
   `lab/web/src/app.css`.
5. The docs site's tokenizer, `www-thanks-computer/scripts/docs-highlight.mjs`.

Five small hand-synced artifacts — still beats one over-engineered code
generator, as long as this list stays honest.

## Regenerate the marketplace icon

If `icons/txcl.svg` changes:

```sh
rsvg-convert -w 256 -h 256 icons/txcl.svg -o icons/icon.png
```

(`brew install librsvg` if you don't have `rsvg-convert`.)

## Add a screenshot

The Marketplace listing benefits from a screenshot in the README. To
add one without depending on the (currently private) repo:

1. Drop the PNG somewhere served at an absolute HTTPS URL — e.g.
   `static/og/txcl-marketplace.png` in `www-thanks-computer`, deployed
   → `https://www.thanks.computer/og/txcl-marketplace.png`.
2. Reference it in `README.md` with the absolute URL.

Once the source repo goes public, a relative `./screenshots/foo.png`
will work too — `vsce` rewrites those to repo raw URLs at publish time.
