# Packages — share a department

_In Thanks, Computer, a stack of rules can run a part of a business —
this page covers packaging a working stack so others can install it.
([Overview](./overview.md))_

A working department — support triage, invoicing, onboarding — is just
a tree of rule files plus the nano-ops they reference. A **package**
makes that tree shareable: versioned, optionally signed, distributable
from a public GitHub repo (zero infrastructure) or any OCI registry
(the same registries that already hold your container images).

```sh
txco install ghcr.io/loremlabs/support-basic --as support
# review what landed in OPS/support/, wire your endpoints, then:
txco apply
```

Install **materializes and stops**: it writes plain, reviewable `.txcl`
files into your workspace and never touches a running chassis. You read
what you're about to run — rules are text, not a black box — fill in
the package's declared external requirements (e.g. *your* notify
endpoint), and deploy with the same `txco apply` you always use.

## Authoring one

A package is an `OPS/`-shaped tree plus a manifest:

```
support-basic/
  txco.package.yaml            # identity, version, exports
  OPS/support/
    0100_TRIAGE/classify.txcl  # EXEC "op://classify"  (bundled nano-op)
    0100_TRIAGE/classify.js    #   ships with the package
    0200_NOTIFY/notify.txcl    # EXEC "op://NOTIFY"    (you supply this one)
```

```sh
txco package init my-dept     # scaffold
txco package validate         # check the tree + manifest
txco package publish --to ghcr.io/you/my-dept --sign
```

Signing uses an ed25519 key (`txco package key generate`);
`txco package inspect <ref> --provenance` verifies it before you
install.

## Why this matters

Most operational knowledge is trapped in one company's glue code. A
package turns "how we triage support" into something you can hand to
another team — or another company — as an installable, inspectable,
upgradeable artifact (`txco package upgrade`). This is where TxCo is
headed: departments you install, not rebuild.
