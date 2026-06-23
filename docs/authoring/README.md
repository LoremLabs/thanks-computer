# Dev Environment — building stacks day to day

Build operation stacks with [Thanks, Computer](https://www.thanks.computer) by organizing a directory of files.

## The op stack layout

An [op stack](../resonators.md) can be created locally as a directory tree of plain files:

```yaml
my-workspace/
  OPS/                           # convention - OPS live here
    support/                     # one directory per stack
      0100_TRIAGE/               # a scope: integer + optional _LABEL
        classify.txcl            # a rule (several .txcl at one scope step run in parallel)
        classify.js              # optional colocated nano-op for op://classify
        mock-request.json        # optional fixtures 
        mock-response.json
        FILES/                   # optional static assets (txco://static)
      0100_OKRS/                 # multiple-directories possible at the same step
        okrs.txcl                # a rule (several .txcl at one scope step run in parallel)
      0200_NOTIFY/
        notify.txcl
      schema.json                # optional — json schema description of the stack others can use
  APPS/                          # optional — local services txco dev boots
    api/server.js                # these do not get deployed in a remote txco chassis
  txco.yaml                      # optional — targets, apps, op:// URLs
```

Scope directories sort the flow (`0100` before `0200`; leading zeros
are cosmetic) — a *scope* is simply a step's address on disk. Step numbers can be sparse and start at 0, flowing to the next largest step.

Folders nested *below* a scope are for organization; the nearest numbered folder sets the step (`0100_TRIAGE/llm/classify.txcl` runs at scope 100).

Stacks whose names start with `_` are system/local
(`_cron`, `_sys/…`) — loaded by the chassis, not pushed by apply.

That tree *is* the flow. The chassis sees this as:

```stack
support
0100 classify okrs
0200 notify
```

## The authoring loop

```sh
txco init support          # scaffold a stack alternative, pull from a registry
txco dev                   # boot apps + a dev chassis, watch, re-apply on save
# …edit, save, curl, read the trace…
txco apply                 # deploy to a cloud chassis (draft + activate per stack)
```

Beyond `apply`: `push`/`pull` for one stack, `draft`/`activate` to
stage and flip deliberately, `versions` + `diff` + `status` for drift
and history, `activate` an older version to roll back — the full verb
table is in the [CLI reference](../advanced/cli.md).

## Develop stacks on the web

You can do many of the things you do via the CLI in the admin interface. 

```sh
txco auth login              # opens the admin UI, authenticated
```

<img width="1437" height="785" alt="image" src="https://github.com/user-attachments/assets/6e7a0d0c-0aa5-4a04-9caf-c436720a0a8e" />


## Sync developer changes

If you're familiar with `git` commands, we borrow much of the same capabilities. 

Stacks change through versioned drafts. Once live they are immutable and a new draft is created for changes.

```sh
# see if your local state diverged with the chassis
txco diff
```

```sh
# pull in changes
txco pull
```
```sh
# push your changes
txco push
```

```sh
# activate the current 
txco activate
```

```sh
# see the current state
txco status
```

## Packages

To make it easy to share a stack, Thanks Computer uses the standard OCI package format that you may be familiar with
if you use `Docker`. We support any package registry such as (GHCR, Docker Hub, ECR, Harbor, self-hosted), and you can use
standard tools like `oras` to inspect the data.

```sh
txco install <ref> --as <stack>
```

For more package commands, see [oci packages](../advanced/txco-oci-packages.md).