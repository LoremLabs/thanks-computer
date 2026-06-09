# support-basic — a TxCo package

A minimal customer-support department, packaged. Three stages over one stack:

| Scope | Dir | Rules | Op refs |
|---|---|---|---|
| 0 | `OPS/support/0000_SETUP/` | `audit`, `enrich` | `op://AUDIT` (required) · `txco://noop` |
| 100 | `OPS/support/0100_TRIAGE/` | `classify` | `op://classify` (**bundled** → `classify.js`) |
| 200 | `OPS/support/0200_NOTIFY/` | `notify` | `op://NOTIFY` (required) |

A **package** is an `OPS/`-shaped tree plus a [`txco.package.yaml`](./txco.package.yaml)
manifest at the root. It is the same shape `bundle.Walk` reads, so install is just
"materialize into `OPS/` then `txco apply`."

## op:// resolution — bundled vs required

- **Bundled** (`op://classify`): the colocated [`classify.js`](./OPS/support/0100_TRIAGE/classify.js)
  travels with the package and is built to wasm at `txco apply` time (TxCo auto-fetches the
  `javy` toolchain on first build — no manual install). Nothing to wire.
- **Required** (`op://AUDIT`, `op://NOTIFY`): external endpoints. `txco install` prints a
  `txco.yaml` `operations:` stub for you to fill in. See
  [`examples/txco.yaml.example`](./examples/txco.yaml.example).

## Try it

```sh
txco package validate ./examples/packages/support-basic
txco inspect dir:./examples/packages/support-basic
txco install dir:./examples/packages/support-basic --as support
# review OPS/support/, wire AUDIT + NOTIFY into txco.yaml, then:
txco apply
```

This package is also the reference fixture for the package-model tests.
