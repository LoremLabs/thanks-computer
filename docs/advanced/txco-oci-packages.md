<!-- nav: Reference -->

# TxCo Packages

A **package** lets you share an operational "department" — a stack of `.txcl` ops — and
install it into another workspace. A package is an `OPS/`-shaped tree plus a
`txco.package.yaml` manifest at its root. Distribute it from a public GitHub repo (zero
infrastructure) or an OCI registry (auth, immutable digests, private repos).

```
support-basic/
  txco.package.yaml            # identity + the op:// resolution contract
  OPS/support/
    0000_SETUP/audit.txcl      # EXEC "op://AUDIT"     (external requirement)
    0100_TRIAGE/classify.txcl  # EXEC "op://classify"  (bundled compute)
    0100_TRIAGE/classify.js    #   the colocated compute, ships with the package
    0200_NOTIFY/notify.txcl    # EXEC "op://NOTIFY"    (external requirement)
```

## 1. Command surface

Consuming a package is part of the everyday workflow; authoring/publishing lives under
`txco package`:

| Command | Flow |
|---|---|
| `txco install <ref> --as <stack>` | registry/package → local `OPS/<stack>/` |
| `txco apply` | local `OPS/` → chassis (active) |
| `txco package init <name>` | scaffold a new package |
| `txco package validate [<dir>]` | validate a package's manifest + tree |
| `txco package inspect <ref>` | show identity + exports (`--provenance` to check the signature) |
| `txco package pull <ref>` | fetch into `.txco/vendor/`, no install |
| `txco package publish --to <oci-ref>` | build + push to a registry (`--sign` to sign) |
| `txco package key generate` | make an ed25519 package-signing keypair |
| `txco package list` | list installed packages (alias: `txco packages`) |
| `txco package upgrade <stack>… \| --all` | re-resolve + re-materialize when a ref's content changed |
| `txco package remove <stack>` | delete `OPS/<stack>/` + drop its lockfile entry |

Install **materializes then stops** — it writes reviewable files into `OPS/` and prints
the next step. You review, wire any external ops (below), then `txco apply` to deploy.
Install never contacts a chassis.

## 2. The manifest (`txco.package.yaml`)

```yaml
apiVersion: thanks.computer/v1alpha1
kind: Package
name: support-basic            # IDENTITY only — registry/namespace are provenance, not here
version: 0.1.0
package:
  kind: department
  install:
    defaultMode: as-stack
    suggestedStack: support
operations:                    # the op:// resolution contract — see §4
  bundled:
    - name: classify
      path: OPS/support/0100_TRIAGE/classify.js
  required:
    - name: AUDIT
      kind: http
      example: https://audit.example.com/op
capabilities: [http.fetch]     # advisory only — nothing enforces
```

The manifest carries **identity** (`name` + `version`). The registry, namespace, and
digest a package came from are **provenance**, derived from the ref it was pulled from and
recorded in the lockfile (§7) — never self-asserted in the manifest, so a copied package
cannot lie about its origin.

## 3. Scopes and stacks

Package content lives under `OPS/<stack>/<scope>/<name>.txcl`, exactly the shape
`txco apply` reads. Scope directories are integers (optionally `_SUFFIX`-annotated, e.g.
`0100_TRIAGE`); multiple `.txcl` files in one scope are parallel rules. v1 install supports
**single-stack** packages; `--as <stack>` renames the exported stack on the way in.

## 4. The op:// resolution contract

Rules reference operations as `op://NAME`. Each ref resolves one of two ways:

- **bundled** — a colocated `<name>.js`/`.ts` sibling next to the `.txcl`. It ships *with*
  the package. List it under `operations.bundled`. Install lays it down; nothing to wire.
  At `txco apply`, the ref becomes `compute://sha256/<digest>`:
  - if the package shipped a prebuilt `<name>.wasm` (see §10), apply uses it directly — **no
    `javy` needed**, and the digest is identical for every consumer (fixed at publish);
  - otherwise apply builds `<name>.js` → wasm locally, **auto-fetching the pinned `javy`
    toolchain on first build** (cached in `~/.config/txco/tools/`; no manual install).
- **required** — an external endpoint with no colocated compute. List it under
  `operations.required`. On install, TxCo **prints** a `txco.yaml` `operations:` stub for
  you to paste and fill in (it never edits `txco.yaml`, to avoid clobbering your comments).

`txco package validate` enforces the split: every `op://NAME` with a colocated file must be
declared `bundled`; every one without must be declared `required`.

## 5. Installing

```sh
txco install support-basic@0.1.0 --as support     # from the default registry (§6)
txco install oci://ghcr.io/you/support-basic:0.1.0 --as support
txco install github:you/txco-packages/support-basic --as support
txco install dir:./examples/packages/support-basic --as support --dry-run
```

Modes: `--as <stack>` (materialize into `OPS/<stack>/`), `--vendor-only` (fetch into
`.txco/vendor/`, no `OPS/` change), `--dry-run` (preview, mutate nothing). Re-installing the
same package to the same stack is idempotent (a content hash gates "no change"), and a
re-install refuses to overwrite a stack you've edited since install — see §8 for the
lifecycle verbs and the local-edit guard.

## 6. Package refs and registry config

User-facing refs map to OCI references:

| You type | Resolves to |
|---|---|
| `sales@v3` | `registry.thanks.computer/txco/sales:v3` (default registry + namespace) |
| `acme/sales@v3` | `registry.thanks.computer/acme/sales:v3` (explicit namespace) |
| `oci://ghcr.io/you/sales:v3` | used verbatim |
| `oci://…@sha256:…` | pinned by digest |

The default registry (`registry.thanks.computer`) and namespace (`txco`) are **baked in**,
so bare refs work with zero config. Override them — or add aliases — in the **workspace**
`txco.yaml` (never in a package manifest):

```yaml
# txco.yaml
registry:
  default: ghcr.io
  defaultNamespace: your-org
  aliases:
    txco: registry.thanks.computer/txco
```

Auth uses your docker credentials (`docker login ghcr.io`), or `TXCO_OCI_USERNAME` /
`TXCO_OCI_PASSWORD`. Public pulls need no auth.

## 7. The lockfile (`txco.packages.lock.yaml`)

Install records provenance in a **committed** `txco.packages.lock.yaml` at the repo root:

```yaml
packages:
  - ref: sales@v3                         # what you typed
    registry: registry.thanks.computer    # provenance, from the resolved ref
    namespace: txco
    name: sales
    version: 3.0.0
    resolved: oci://registry.thanks.computer/txco/sales@sha256:…   # the digest pin
    installedAs: sales
    mode: as-stack
    installedAt: "2026-05-31T12:00:00Z"
```

This is workspace **provenance** ("where these files came from") — deliberately separate
from the chassis's server-side version lineage ("what's deployed"). The committed
`OPS/` files are authoritative; the lockfile drives reproducibility and future upgrades.

## 8. Managing installed packages

The lockfile (§7) tracks what's installed, so TxCo can show, update, and remove packages:

```sh
txco package list                          # what's installed (alias: txco packages)
txco package list --json                   # machine-readable
txco package upgrade support               # re-resolve support's ref, re-materialize if changed
txco package upgrade --all
txco package remove support                # delete OPS/support/ and drop the lockfile entry
txco package remove support --keep-files   # drop the entry only; leave the files
```

`list` flags an **edited** stack — one whose `OPS/<stack>/` files no longer match what was
installed (compared by the same `.txcl` + mock content hash the lockfile pins; edits to a
colocated `.js` or to docs aren't tracked):

```
NAME           VERSION  INSTALLED-AS  MODE      DIGEST        EDITED?
support-basic  0.1.0    support       as-stack  b2e046bde6e5  yes
```

**Upgrade re-pulls whatever the recorded ref points to now.** A ref pinned to a fixed
version (`sales@3.0.0`) stays put — `upgrade` reports "up to date"; a moving ref (`sales`,
`sales@latest`) or a `dir:`/`github:` source picks up new content, re-materializes
`OPS/<stack>/`, and re-pins the lockfile digest + version. To jump to a *different* version,
re-install over the stack: `txco install sales@4.0.0 --as sales`. `--all` upgrades every
installed stack and continues past any that fail, reporting a summary.

**Local edits are protected.** `upgrade`, `remove`, and re-`install` all refuse to overwrite
a stack you've edited since install — run `txco diff` to inspect, or pass `--force` to
discard the edits (for `remove`, `--keep-files` drops only the lockfile entry and leaves the
files untouched). `--dry-run` previews any of these; `--force --dry-run` previews past the
guard.

## 9. Validation

`txco package validate` (and install/publish) run **Go-code validation** that is
authoritative: it checks the header, semver, that every rule parses, that bundled compute
files exist, and the §4 bundled-vs-required contract. A JSON Schema
(`examples/packages/txco.package.v1alpha1.schema.json`) ships for editor autocompletion
only — it is **not** loaded by the binary.

## 10. Publishing

```sh
txco package validate ./packages/sales
txco package publish --to oci://ghcr.io/you/sales:3.0.0 ./packages/sales
# → prebuilt 1 compute(s) (javy plugin 8.1.1)
#   published oci://ghcr.io/you/sales@sha256:…
```

Publish validates, packs the tree into a single-layer OCI artifact, pushes it, and prints
the resolved digest. Tags are convenience; the digest is truth.

**Prebuilt wasm.** Publish auto-builds each bundled compute (`<name>.js` → `<name>.wasm`)
into the published artifact — fetching the pinned `javy` toolchain automatically if it isn't
already present (cached in `~/.config/txco/tools/`). Your source tree stays `.js`-only (the
build happens in a staging copy; nothing to commit). Consumers then `apply` with **no
toolchain**, and every consumer gets the identical `compute://sha256/<digest>` (the digest is
fixed at publish, not recomputed per machine). The `.js` source still ships alongside, for
transparency and as a build-from-source fallback (§4).

- `--no-prebuild` ships source-only (consumers build at apply, auto-fetching `javy` then).
- If `javy` can't be obtained at publish (offline with `TXCO_JAVY_NO_DOWNLOAD`, or an
  unsupported platform), it's a heads-up, not a failure — the package ships source-only.
- The wasm is dynamically linked against the chassis's vendored javy plugin; publish names the
  plugin version it built against. A chassis with an incompatible plugin reports it at apply.
- The wasm rides inside the package layer, so an ed25519 signature (§11) covers it too.

## 11. Signing and trust

Signing lets a consumer prove **who** built a package, not just that the bytes are pinned.
TxCo uses a self-contained **ed25519** scheme — no external tools, no sigstore/cosign
dependency.

```sh
# Author: make a keypair once, then sign on publish.
txco package key generate                       # → ~/.config/txco/keys/signing.ed25519 (+ .pub)
txco package publish --to oci://ghcr.io/you/sales:3.0.0 --sign ./packages/sales
# → published oci://ghcr.io/you/sales@sha256:…
#   signed by SHA256:… (sha256-…​.sig)

# Consumer: trust the author's public key, then require a signature.
# txco.yaml:
#   trust:
#     keys:
#       - name: acme
#         pubkey: "ssh-ed25519 AAAA…"          # the line key generate printed
txco install sales@3.0.0 --as sales --require-signature
# → verified: signed by SHA256:…
txco package inspect sales@3.0.0 --provenance   # show the signature without installing
```

How it works:

- The signature is a small OCI artifact in the **same repository**, found by the cosign tag
  convention `sha256:<hex>` → `sha256-<hex>.sig`. Its layer is the exact signed JSON payload;
  the ed25519 signature, key id (`ssh-keygen` SHA256 fingerprint), and public key are
  annotations. (Tag-based, so it works on any registry — the Referrers API isn't required.)
- Verification checks the signature over the stored payload bytes, then binds the payload to
  the **pulled digest** and **repository** — a signature can't be transplanted onto different
  content or copied under another name.
- **Posture:** without `--require-signature`, an unsigned or untrusted package installs but
  prints a warning; `--require-signature` fails closed (nothing is written to `OPS/`) unless
  the package is signed by a key in `trust:` (or passed via `--key`). A verified install
  records the signer key id in the lockfile (`signedBy:`).
- **No key is trusted by default** — the default registry/namespace is a convenience, not a
  trust boundary. Trust is whatever you list in `trust:`.

> The signature format is txco-native (ed25519), **not** cosign-compatible — it's verified by
> `txco`, not by `cosign verify`. Trust keys are ed25519 public keys (an `ssh-ed25519` line,
> a `.pub` path, or base64); the key id is the `ssh-keygen -lf` fingerprint.

## 12. OCI artifact format

A package is a standard OCI artifact:

- **config** = the verbatim `txco.package.yaml`, media type
  `application/vnd.thanks.computer.package.manifest.v1alpha1+yaml`.
- **layer** (one) = `gzip(tar(tree))`, media type
  `application/vnd.thanks.computer.package.layer.v1alpha1.tar+gzip`.
- **artifactType** = `application/vnd.thanks.computer.package.v1alpha1`.

Any OCI registry (GHCR, Docker Hub, ECR, Harbor, self-hosted) can store it; standard tools
(`oras`) can inspect it. 