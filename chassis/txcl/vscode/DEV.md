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

Each release:

```sh
npx @vscode/vsce login loremlabs                    # one-time, paste PAT
npx @vscode/vsce publish patch | minor | major      # bumps version + publishes
```

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
lexer at `chassis/txcl/lexer/lexer.go` and the token table at
`chassis/txcl/token/token.go`. When those change, update in lockstep:

1. This grammar.
2. The TS port at `admin-ui/src/lib/txcl/lexer.ts` + `tokens.ts` (which
   has a Go-output parity test in `lexer.test.ts`).

Three small artifacts, periodically synced — beats one over-engineered
code generator.

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
