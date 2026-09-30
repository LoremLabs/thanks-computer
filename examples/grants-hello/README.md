# grants-hello — open a sandbox for a command, and decide each time

A stack that declares **sandboxes** — named bundles of what a program may
hold — dispatches a command to a workspace with a **run grant** that may
open them, and a `_grant` stack that decides each secret a sandbox names.

```
OPS/grants-demo/
  SANDBOXES/open.yaml     DEMO ← DEMO_OPEN        what a program that opens `open` is handed
  SANDBOXES/closed.yaml   DEMO ← DEMO_CLOSED
  SANDBOXES/by_rule.yaml  DEMO ← DEMO_BY_RULE
  SANDBOXES/never.yaml    DEMO ← DEMO_NEVER
  SANDBOXES/all.yaml      DEMO ← DEMO_OPEN, DEMO_CLOSED ← DEMO_CLOSED
  100/principal.txcl    POST /setup             the principal the work acts for
  110/grant_*.txcl                              four standing grants: it may ever be handed these
  120/mint.txcl         GET  /run /launch       a run grant for this piece of work, naming the sandboxes
  130/revoke.txcl       ?revoked=1              …revoked before the command runs
  140/run.txcl          GET  /run?sandbox=…     the chassis opens the sandbox, then starts the command
  140/launch.txcl       GET  /launch?sandbox=…  the command opens it itself: `txco sandbox NAME -- program`
  140/journal.txcl      GET  /journal           what _grant/0 recorded
  300/respond.txcl
OPS/_grant/
  0/journal.txcl        every request → notebook/append
  10/policy.txcl        allows a secret its pull policy closed, through one sandbox
  10/never.txcl         refuses a secret its pull policy opened
```

## Run it

```sh
txco dev --allow-local-workspace --grant    # from this directory
```

`--grant` is the Unix socket `txco sandbox` reaches the chassis on, for
`/launch`. `/run` needs only `--allow-local-workspace`.

Store four secrets, and open two of them:

```sh
for n in DEMO_OPEN DEMO_CLOSED DEMO_BY_RULE DEMO_NEVER; do
  printf 'the value of %s' "$n" | txco auth tenant secrets set "$n" dev
done
txco auth tenant secrets policy DEMO_OPEN  --pull any dev
txco auth tenant secrets policy DEMO_NEVER --pull any dev
curl -X POST localhost:8080/setup
```

## What happens

| Request | Pull policy | `_grant` rule | The command |
|---|---|---|---|
| `/run?sandbox=open` | `any` | none | sees 22 bytes in `DEMO` |
| `/run?sandbox=closed` | `none` | none | never starts: `grant_refused`, `sandbox "closed" was refused` |
| `/run?sandbox=by_rule` | `none` | allows it | sees 25 bytes in `DEMO` |
| `/run?sandbox=never` | `any` | refuses it | never starts |
| `/run?sandbox=all` | `any`, `none` | none | never starts: `DEMO_OPEN` was released and charged, then `DEMO_CLOSED` refused |
| `/run?sandbox=missing` | | | never starts: the grant does not name it |
| `/run?sandbox=open&revoked=1` | `any` | | never starts: the grant is handed to no command |

`/launch` answers the same for each, with a program that finds the secret
in `DEMO` — and `TXCO_RUN_GRANT` not set — or `txco sandbox` exiting 77.

- **The program never names a secret.** It names a sandbox; the stack said
  what that sandbox sets when it deployed the file. The run grant copied
  the sandboxes it names when it was minted, so a later edit to a file
  changes new mints, not live grants.
- **A sandbox opens whole or not at all.** `all` names one open secret and
  one closed: the open one is released and charged, the closed one is
  refused, and the command is handed nothing.
- **Every refusal looks the same to the command.** A secret that is closed,
  one a rule refused and a sandbox the grant does not name all leave the
  command unrun, or `txco sandbox` at exit 77. The reason is in the trace:
  `txco trace`.
- **The chassis proposes, the stack disposes.** With `OPS/_grant/10`
  removed, the pull policy alone decides.
- **The commands print how much they hold, never what.** Whatever a command
  prints lands in the envelope and the trace.

```sh
curl localhost:8080/journal     # one entry for each secret asked for, with its sandbox and the chassis's proposal
txco trace                      # the _grant run itself: the request, the proposal, the verdict
```

See `docs/advanced/grants.md`.
