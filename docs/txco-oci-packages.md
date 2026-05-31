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
| `txco package inspect <ref>` | show identity + exports, no install |
| `txco package pull <ref>` | fetch into `.txco/vendor/`, no install |
| `txco package publish --to <oci-ref>` | build + push to a registry |

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
capabilities: [http.fetch]     # advisory only — nothing enforces these in v1
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
  the package and is built to wasm at `txco apply` time (needs `javy` on PATH). List it
  under `operations.bundled`. Install lays the source down; nothing to wire.
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
same package to the same stack is idempotent (a content hash gates "no change").

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

## 8. Validation

`txco package validate` (and install/publish) run **Go-code validation** that is
authoritative: it checks the header, semver, that every rule parses, that bundled compute
files exist, and the §4 bundled-vs-required contract. A JSON Schema
(`examples/packages/txco.package.v1alpha1.schema.json`) ships for editor autocompletion
only — it is **not** loaded by the binary.

## 9. Publishing

```sh
txco package validate ./packages/sales
txco package publish --to oci://ghcr.io/you/sales:3.0.0 ./packages/sales
# → published oci://ghcr.io/you/sales@sha256:…
```

Publish validates, packs the tree into a single-layer OCI artifact, pushes it, and prints
the resolved digest. Tags are convenience; the digest is truth.

## 10. OCI artifact format

A package is a standard OCI artifact:

- **config** = the verbatim `txco.package.yaml`, media type
  `application/vnd.thanks.computer.package.manifest.v1alpha1+yaml`.
- **layer** (one) = `gzip(tar(tree))`, media type
  `application/vnd.thanks.computer.package.layer.v1alpha1.tar+gzip`.
- **artifactType** = `application/vnd.thanks.computer.package.v1alpha1`.

Any OCI registry (GHCR, Docker Hub, ECR, Harbor, self-hosted) can store it; standard tools
(`oras`, `cosign`) can inspect and, later, sign it.
