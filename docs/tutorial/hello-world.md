# Hello, world in the cloud

_Deploy your first stack to the hosted service — no local runtime. You sign in,
pull a published package, deploy it, and get a public URL back. About two minutes.
([Quickstart](../quickstart.md) first if you haven't; [Overview](../overview.md) for the why.)_

Everything here runs on **[Thanks, Computer](https://www.thanks.computer)**, the hosted
service. The `txco` CLI on your machine is the remote control: it signs you in, pulls a
package, and pushes it to your cloud tenant. Nothing executes locally — there's no
`txco serve`, no chassis to run.

:::note
**Cloud or local?** This tutorial uses our hosted chassis — it has a free tier, needs no
setup, and gives your stack a public URL. To run the very same stack on your own machine
instead, skip `txco login` and use **`txco dev`**: it boots a self-contained local chassis,
applies your `OPS/`, and serves the stack at a printed `http://hello-<rand>.localhost:8099`.
Same package, same rules — just no account and a local URL.
:::

## 1. Install the CLI

```sh
brew tap loremlabs/txco && brew install txco
```

or

```sh
curl -fsSL https://get.thanks.computer/install.sh | bash
```

## 2. Sign in

```sh
txco login
```

Your browser opens to sign in. The first time, you pick a **tenant** slug — your workspace
handle in the cloud — and the service provisions it and enrolls a signing key on this
machine. From now on, `txco` commands target *your* cloud tenant.

## 3. Pull a published package

A **[package](../packages.md)** is a shareable stack: a tree of rules, versioned and
published to a registry. We'll pull `hello-world` — a single rule that returns a JSON
greeting.

```sh
mkdir hello && cd hello
txco install hello-world --as hello
```

```
  ✔ verified: signed by txco [SHA256:Zzh39Nrs…]
installed hello-world 0.1.1 as stack "hello" (1 file)

Review OPS/hello/, then run `txco apply` to deploy.
```

The `✔ verified` line means the package's signature checked out — the CLI fetched the
registry's signing key automatically (from its `.well-known`), so there's nothing for you
to configure. Install then **materializes and stops**: it writes plain, reviewable text
into your workspace and never touches the cloud. Rules are text, not a black box — read
what you're about to run:

```sh
cat OPS/hello/100/hello.txcl
```

```txcl
WHEN @src == "http"
  EMIT .say = "hello world"
```

Fire on any web request (`@src == "http"`) and set `.say`. The web inlet returns the
envelope as JSON (its default projection — every key except `_`-prefixed internals), so
the response is `{"say":"hello world"}`. No external services to wire — it deploys as-is.

## 4. Deploy to the cloud

```sh
txco apply
```

`apply` pushes the stack to your tenant and activates it, then prints the public URL the
service minted:

```
hello v1 activated (1 files)
  → https://hello-a1b2c3.stacks.thanks.computer
```

## 5. Visit it

```sh
curl https://hello-a1b2c3.stacks.thanks.computer
```

```json
{"say":"hello world"}
```

Open it in a browser too — it's live on the public internet, served from your cloud
tenant.

## Change it

Edit the rule and ship again — the same one command:

```sh
# edit OPS/hello/100/hello.txcl …
txco apply
```

Each `apply` is a new version; the URL stays the same.

## What you just did

You deployed a stack to the hosted service without running anything locally — sign in,
pull a package, apply, done. The CLI held your files; the runtime was ours. From here:

- **[Packages](../packages.md)** — pull other published stacks, or bundle and publish your own.
- **[CLI reference](../advanced/cli.md)** — `txco login`, `cloud`, profiles, and the rest.
- More tutorials are coming — real ingress, AI as an operation, and a human in the loop.
