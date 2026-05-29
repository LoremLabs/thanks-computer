# Example templates

This directory holds reference templates for `txco init --from`. Copy any of these subdirectories into a separate GitHub repo (`yourorg/txco-templates`) to make them available remotely:

```sh
git init yourorg-txco-templates && cd yourorg-txco-templates
cp -r ../thanks-computer/examples/templates/support-basic .
git add . && git commit -m 'initial template' && git push
```

Then anyone can scaffold from it:

```sh
txco init support --from github:yourorg/txco-templates/support-basic
```

A template subdirectory is a **flat tree** of `<scope>/<name>.txcl` files (plus optional `mock-request.json` / `mock-response.json` siblings, plus optional sub-stack subdirectories). Whatever's inside the template subdirectory lands directly under `OPS/<stack>/` in the user's workspace; `txco init` supplies the `OPS/` wrapper and the stack name.

Scope dirs can be bare integers (`100/`), zero-padded (`0100/`), or zero-padded with a descriptive suffix (`0100_TRIAGE/`). Multiple `*.txcl` files in the same scope dir are parallel rules at that stage — the chassis runs them concurrently and deep-merges their responses.

There is no APPS/ directory in v2 — the chassis dispatches to existing HTTP endpoints rather than generating or deploying services.

## Available templates here

- [`support-basic/`](./support-basic) — a 2-scope flow (classify → route) plus a `triage/` sub-stack override for high-priority cases.
