# Share Stacks 

In [Thanks, Computer](https://www.thanks.computer) you can share and use stacks others have published.

A working department — support triage, invoicing, onboarding — is a
playbook for a kind of work: a tree of rule files plus
the nano-ops they reference. A **package**
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

## Bundling your package

A package is an `OPS/`-shaped tree plus a manifest:

```
support-basic/
  txco.package.yaml            # identity, version, exports
  OPS/support/                 # Your op stack here
    0100_TRIAGE/classify.txcl  
    0100_TRIAGE/classify.js    
    0200_NOTIFY/notify.txcl
    schema.json                # good practice, but optional
```

What you're installing, as your op stack:

```stack
support-basic
0100 classify
0200 notify
```

```sh
txco package validate         # check the tree + manifest
txco package publish --to ghcr.io/username/support-basic --sign
```


## Installing your shared package

Others can then install your op stack with:

```sh
txco package inspect ghcr.io/username/support-basic
txco package install ghcr.io/username/support-basic --as support
```

And they can then modify it to meet their needs.
