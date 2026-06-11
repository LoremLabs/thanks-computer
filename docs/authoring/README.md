# Authoring — building stacks day to day

_The working guides for stack builders: the workspace layout, the dev
loop, mocks, and nano-ops. Concepts live in the
[main docs](../README.md); these pages are the workflow._

## The workspace

A stack is a directory tree of plain files:

```
my-workspace/
  txco.yaml                      # optional — targets, apps, op:// URLs
  OPS/
    support/                     # one directory per stack
      0100_TRIAGE/               # a scope: integer + optional _LABEL
        classify.txcl            # a rule (several .txcl at one scope run in parallel)
        classify.js              # optional colocated nano-op for op://classify
        mock-request.json        # optional fixtures — see mocks.md
        mock-response.json
        FILES/                   # optional static assets (txco://static)
      0200_NOTIFY/
        notify.txcl
  APPS/                          # optional — local services txco dev boots
    api/server.js
```

Scope directories sort the flow (`0100` before `0200`; leading zeros
are cosmetic) — a *scope* is simply a step's address on disk. Stacks
whose names start with `_` are system/local
(`_cron`, `_sys/…`) — loaded by the chassis, not pushed by apply.

That tree *is* the flow. The chassis sees it as:

```stack
support
0100 classify
0200 notify
```

## The loop

```sh
txco init support          # scaffold a stack
txco dev                   # boot apps + a throwaway chassis, watch, re-apply on save
# …edit, save, curl, read the trace…
txco apply                 # deploy to a real chassis (draft + activate per stack)
```

Beyond `apply`: `push`/`pull` for one stack, `draft`/`activate` to
stage and flip deliberately, `versions` + `diff` + `status` for drift
and history, `activate` an older version to roll back — the full verb
table is in the [CLI reference](../advanced/cli.md).

## The guides

- **[dev.md](./dev.md)** — what `txco dev` boots, watches, and applies;
  the `txco.yaml` schema.
- **[mocks.md](./mocks.md)** — develop a flow before its services
  exist; mock fixtures, request-scoped mock patterns, and the
  production `mock: deny` policy.
- **[nano-ops.md](./nano-ops.md)** — the `op://` lifecycle:
  init → run → test → apply.

Related: [TXCL](../txcl.md) for the language,
[envelope.md](../advanced/envelope.md) for every field you can read
and write, [schemas](../schemas.md) for declaring your stack's shape.
