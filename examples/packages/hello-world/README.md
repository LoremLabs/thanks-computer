# hello-world — a TxCo package

The smallest useful stack: one rule that returns a **`{"say":"hello world"}`** JSON response.

| Scope | Dir | Rule | Op refs |
|---|---|---|---|
| 100 | `OPS/hello/100/` | `hello` | none — pure txcl |

A **package** is an `OPS/`-shaped tree plus a [`txco.package.yaml`](./txco.package.yaml)
manifest at the root. Because the rule references no `op://`, there is **nothing to
build and nothing to wire** — install materializes one file and `txco apply` deploys it.
That makes this the package the [cloud tutorial](../../../docs/tutorial/hello-world.md) pulls.

## The whole stack

[`OPS/hello/100/hello.txcl`](./OPS/hello/100/hello.txcl):

```txcl
WHEN @src == "http"
  EMIT .say = "hello world"
```

It fires on any web request (`@src == "http"`) and sets `.say`. The web inlet returns
the envelope as JSON — its default projection, every key except `_`-prefixed internals —
so the response is `{"say":"hello world"}`. No render rule, no external services.

## Try it

```sh
txco package validate ./examples/packages/hello-world
txco install dir:./examples/packages/hello-world --as hello
txco apply
```

In the cloud, `txco apply` activates the stack and prints its public URL
(`https://hello-<rand>.stacks.thanks.computer`). Locally, `txco dev` serves it at
`http://localhost:8080/`.

## Published

This package is published (signed) to the blessed registry namespace, so the tutorial
installs it by bare name and the signature verifies automatically — the CLI fetches the
registry's signing key from `https://registry.thanks.computer/.well-known/txco-signing-keys.json`:

```sh
txco install hello-world --as hello   # → oci://registry.thanks.computer/txco/hello-world:latest
#   verified: signed by SHA256:Zzh39Nrs…
```
