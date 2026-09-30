<!-- nav: Grants -->

# Grants — what a principal, and one piece of its work, may hold

_A stack dispatches work somewhere else: a command in a
[workspace](../workspaces.md), acting for a [principal](./users.md#principals).
That work may need a secret. Grants say what it may be handed by this
chassis, and sandboxes say what it holds._

```text
   principal
       │
  standing grants        may ever be handed GITHUB_PAT, DB_DSN, …      txco://grant/put
       │
   run grant             may open the sandboxes github, postgres       txco://delegate/mint
       │
  ┌────┴──────────────────────────────┐
  │ exec WITH grant, sandbox = github │ the chassis opens it, then starts the command
  │ txco sandbox github -- gh pr list │ the program asks its chassis to open it
  └────┬──────────────────────────────┘
       │ one request per secret, decided by the tenant's _grant stack
       ▼
   PROGRAM     GH_TOKEN=…      authority stops here
```

| To | Read |
|---|---|
| See it whole | [A complete example](#a-complete-example) |
| Say what a program may hold | [Sandboxes](#sandboxes) |
| Say what a principal may ever be handed | [Standing grants](#standing-grants) |
| Say what one piece of work may open | [Run grants](#run-grants) |
| Start a command inside a sandbox | [Opening a sandbox for a command](#opening-a-sandbox-for-a-command) |
| Let a program open one itself | [`txco sandbox`](#txco-sandbox) |
| Decide which requests are allowed | [What decides a request](#what-decides-a-request) |
| Turn it on | [Turning it on](#turning-it-on) |

## Who ends up holding what

**The secret goes to the program, in cleartext,** as the value of a
variable the sandbox names. It goes nowhere else.

| | holds | never holds |
|---|---|---|
| The rule that dispatched the work | The grant's id, the sandboxes' names | The token, the secret |
| The envelope and the trace | Ids, the sandbox, the variable, the request, the proposal, the verdict | The secret, the token |
| The command | What its sandboxes set, and — where it may open more — its token | A secret it chose by name: there is no way to ask for one |

This is for work the chassis does not run itself: a program in a workspace
that has to hold the connection. A rule that needs a secret for its own
call does not need a grant: it names the secret with `secrets.env.*` or
`secrets.headers.*`, and the chassis uses it where it is stored
([workspaces](../workspaces.md#exec--the-request), [secret store](./runbook-secret-store.md)).

## A complete example

A stack deploys by pushing a repository from a workspace. The push needs a
token, which the program running `git` must hold.

Once, in the stack's tree — what a deploy step holds, under which name:

```yaml
# OPS/release/SANDBOXES/github.yaml
description: what a deploy step holds
env:
  GH_TOKEN: secret:GITHUB_PAT
```

Once, when the principal is set up:

```txcl
# `service:release` may ever be handed GITHUB_PAT
EXEC "txco://grant/put"
  WITH principal = "service:release", kind = "secret", name = "GITHUB_PAT"
```

Once, by the operator, since the secret's own policy starts closed:

```sh
txco auth tenant secrets policy GITHUB_PAT --pull reviewed
```

On every deploy:

```txcl
# 3400: what this deploy may open, and where it runs
WHEN ._deploy.id != ""
  EXEC "txco://delegate/mint"
    WITH principal  = "service:release",
         run        = ._deploy.id,
         allow      = ["github"],
         workspace  = "build",
         node_class = "reviewed",
         ttl        = 600,
         budget     = 3,
         into       = "_delegate"

# 3410: run it inside the sandbox
WHEN ._delegate.id != ""
  EXEC "workspace://build/exec"
    WITH grant   = ._delegate.id,
         sandbox = "github",
         command = "git push origin main",
         into    = "_push"
```

`git` runs with `GH_TOKEN` in its environment and nothing else of the
grant's. The rule sees `._push.exit` and `._push.stdout`; the trace shows
that `GITHUB_PAT` was asked for through `github`, and allowed, by which
run, and never its value.

The same, for a program that decides on its own when it needs the token —
an agent loop, a script with many steps:

```txcl
    WITH grant   = ._delegate.id,
         command = "./release.sh"
```

```sh
# inside release.sh
"$TXCO_BIN" sandbox github -- git push origin main
```

To refuse the deploy's token outside working hours, or to keep a record of
every release, add a rule to the tenant's `_grant` stack
([the grant inlet](./protocols/grant.md)). To stop a deploy that is still
running, revoke its grant: its next request is refused.

There are two kinds of grant, and the second can only narrow the first.

| | Standing grant | Run grant |
|---|---|---|
| **Says** | This principal may ever be handed this | This piece of work may open these sandboxes, now |
| **Lasts** | Until revoked | Minutes: it expires, and ends with the work |
| **Written by** | `txco://grant/put` | `txco://delegate/mint` |
| **Holds** | One principal, one resource, its verbs | Its sandboxes, a budget, an expiry, a workspace |

A grant names one of two kinds of resource:

| kind | named by | verb | the principal may |
|---|---|---|---|
| `capability` | lowercase words joined by dots: `crm.lookup` | `invoke` | call it |
| `secret` | a secret's name: `CRM_KEY` | `release` | be handed the secret itself |

A name is exact. There are no wildcards and no families: `crm.lookup` does
not grant `crm.update`.

## Sandboxes

A sandbox is what a program may hold: a small file the stack deploys, one
per name, under the reserved `SANDBOXES/` directory beside `OUTLETS/` and
`DATASETS/`.

```yaml
# OPS/<stack>/SANDBOXES/<name>.yaml
description: what a deploy step holds     # optional
env:
  GH_TOKEN: secret:GITHUB_PAT             # VARIABLE: reference
  GH_HOST:  secret:GH_ENTERPRISE_HOST
```

| | |
|---|---|
| **The name** | The file's stem: `[a-z][a-z0-9_-]*`, at most 64. It appears in `allow`, in `WITH sandbox`, on the command line. |
| **`env`** | Required. Each entry sets one variable to one secret. A variable is `[A-Za-z_][A-Za-z0-9_]*`; a reference is `secret:<NAME>`. One secret may fill several variables. A name starting `TXCO_` is the chassis's own. At most 32. |
| **`description`** | Optional, for whoever reads the file. |
| **Anything else** | A deploy error. Only what the chassis enforces is declared. |

- **It deploys with the stack.** `txco apply` uploads it, `txco lint` and
  the activation gate parse it, and a file that does not parse refuses the
  deploy. A canary slot declares its own, as it does its outlets.
- **The program never names a secret.** It names a sandbox; the stack said
  what that sets when the file deployed. Nothing in the chassis hands a
  program a secret by name.
- **A run grant copies its sandboxes at mint.** Editing a file changes new
  mints, not live grants. Every request still reads the grant's row, so
  revoking it, closing it or spending its budget bites as before.
- **A sandbox opens whole or not at all.** Each secret it names is decided
  on its own, in the trace on its own, charged on its own. One refused, and
  the program is handed nothing; what was already released is discarded and
  what was charged stays charged.
- **The `_grant` stack sees which sandbox and which variable**
  (`@grant.sandbox`, `@grant.env`) beside the secret's name, so a rule may
  allow a secret through one sandbox and refuse it through another.

`files:`, `network:` and `capabilities:` are not declared yet: a sandbox
holds only what the chassis enforces.

## Standing grants

```txcl
# research may be handed the CRM key, and may call crm.lookup
WHEN ._setup.ok == true
  EXEC "txco://grant/put"
    WITH principal = "service:research", kind = "secret", name = "CRM_KEY"
# → _grant = {id, principal, kind, name, verbs, created_by, created_at, created}
```

| op | WITH | result |
|---|---|---|
| `grant/put` | `principal`, `kind`, `name`, `verbs?` | the grant, and `created` |
| `grant/list` | `principal`, `kind?`, `include_revoked?` | `{principal, count, items[]}`, newest first. The owning stack only. |
| `grant/list` | `kind`, `name` | `{kind, name, count, items[]}`: who holds this resource. Any stack in the tenant. |
| `grant/revoke` | `id` (with `principal` to pin whose it must be), or `principal`, `kind`, `name` | the grant, and `revoked` |

All three answer at `into` (default `_grant`).

- **`put` is safe to retry.** Putting a grant that already holds returns it
  with `created: false`.
- **The principal must already exist.** It needs a user record, a username
  or a credential first. A grant cannot name a principal that another
  stack could claim later.
- **The resource need not exist.** A grant may be written before the
  secret it names is stored.
- **A grant has no conditions.** No roles, no inheritance, no time limit.
  It is a row a person can read aloud.
- **A disabled user holds nothing** while disabled. Nothing is revoked, so
  enabling the user restores every grant.
- **Pass `principal` with an `id` that came from a request**, for the
  reason [credentials do](./users.md#revoking-one-device).
- A principal may hold 1024 live grants.

**A standing grant decides nothing on its own.** It is the outer bound. A
run grant names what one piece of work may open, and [each request is
decided when it is made](#what-decides-a-request).

## Run grants

To delegate is to hand part of what a principal may be handed to one piece
of its work. The record of it is a run grant. Mint one when you dispatch
the work:

```txcl
# 3400: this task may open two sandboxes
WHEN ._task.id != ""
  EXEC "txco://delegate/mint"
    WITH principal = "service:research",
         run       = ._task.id,
         allow     = ["crm", "warehouse"],
         workspace = "bench",
         ttl       = 900,
         budget    = 20
# → _delegate = {id, principal, run, generation, workspace, workspace_id,
#                node_class, allow, sandboxes, budget, spent, depth,
#                minted_by, issued_at, expires_at, live}
```

| op | WITH | result |
|---|---|---|
| `delegate/mint` | `principal`, `run`, `allow`, and the optional params below | the grant |
| `delegate/get` | `id` | the grant, what it has `spent`, and whether it is `live` |
| `delegate/revoke` | `id` | the grant, and `revoked` |
| `delegate/close` | `id`, `reason?` (default `done`) | the grant, and `closed` |

All four answer at `into` (default `_delegate`). Only the stack that minted
a grant may read, revoke or close it.

| mint param | meaning | default |
|---|---|---|
| `run` | The name of the work: a task id, a job id. Letters, digits and `. _ @ + : / -`. | required |
| `allow` | The sandboxes the work may open, by name: each a `SANDBOXES/<name>.yaml` of this stack. 1 to 16. | required |
| `workspace` | A workspace of the calling stack, by name: where the work is sent. It need not exist yet. | none |
| `node_class` | `reviewed` or `unreviewed`: what is allowed to run there | `unreviewed` |
| `ttl` | Seconds the grant lasts | `--run-grant-ttl-default` |
| `budget` | Requests the work may make: one per secret opened | `--run-grant-budget-default` |
| `generation` | A counter of your own, such as a lease. It must be later than any the run has had. | the next one |
| `parent` | The id of a run grant to narrow from | none |

In the result, `allow` is every secret the sandboxes name, and `sandboxes`
is each sandbox's variable names — never a value.

**There is no token in the result.** The token that travels with the work
is signed by the chassis when it hands the work over, and never enters an
envelope or a trace. A rule holds the grant's `id`, which names the grant
and authorizes nothing.

### A run grant only narrows

| Rule | Refused as |
|---|---|
| Every sandbox is one the stack declares | `no_sandbox`; the message names what the stack has |
| Every secret the sandboxes name is one the principal holds a standing grant for | `exceeds_standing`; the message names what is missing |
| A grant narrowed from a `parent` names only secrets the parent names, whatever it calls its sandboxes | `exceeds_parent` |
| Its budget comes out of the parent's, all of it, at mint | `exceeds_parent` |
| It expires no later than the parent. A longer `ttl` is shortened, not refused. | |
| It is narrowed at most 4 times | `depth` |
| Only the stack that minted the parent narrows it | `not_owner` |

Work that a run dispatches needs a `run` name of its own, such as
`task-42/lookup`. The principal may differ from the parent's: that is how
one principal hands part of its work to another.

### What ends a run grant

| Event | The row shows |
|---|---|
| It expires | `expires_at` in the past |
| `delegate/close`: the work ended | `closed_at`, and your `close_reason` |
| `delegate/revoke` | `revoked_at` |
| The same `run` is minted again | `closed_at`, `close_reason: "superseded"` |
| Its budget is spent | `spent.calls` equals `budget.calls` |

- **Ending a grant ends every grant narrowed from it.**
- **Every request reads the row**, so each of these takes effect on the
  work's next request.
- **Minting a run again replaces its grant.** The newest generation is the
  only live one, so a stale copy of the work is refused. Two runs on one
  workspace each keep their own grant.
- **Nothing extends a grant.** For longer work, mint again.
- **Ending a grant recalls nothing.** It stops what the work may still ask
  for. A secret the work was already handed stays where it is until you
  rotate it.

### The node class

`node_class` says what is allowed to run where the work is sent.

| class | what runs there |
|---|---|
| `unreviewed` | Anything, including code written a moment ago |
| `reviewed` | Only what was reviewed before it was installed |

It is a promise by whoever mints the grant. The chassis cannot prove what
runs on a machine. A secret's [pull policy](#the-pull-policy) is checked
against it.

## Opening a sandbox for a command

```txcl
# 3410: run the work inside its sandboxes
WHEN ._delegate.id != ""
  EXEC "workspace://bench/exec"
    WITH grant   = ._delegate.id,
         sandbox = ["crm", "warehouse"],
         command = "./build.sh"
```

`grant` hands one command the run grant; `sandbox` — one name or a list —
says which of its sandboxes the chassis opens before the command starts.
The command starts with every variable they set, or does not start.

Where the command runs on the chassis's own machine (the local provider),
it also gets what it needs to open sandboxes itself at run time:

| variable | holds |
|---|---|
| `TXCO_GRANT_SOCK` | Where `txco sandbox` reaches the chassis. Set where the `grant` personality is on. |
| `TXCO_RUN_GRANT` | The grant's token. `txco sandbox` presents it. |
| `TXCO_RUN` | The run's name |
| `TXCO_BIN` | The `txco` binary, for `"$TXCO_BIN" sandbox …` |

- **The values travel in the exec.** Opening happens here, in the chassis,
  so the push form works on any provider — the fleet's included — as
  `secrets.env.*` does. A provider whose commands run on another machine
  gets the sandboxes' variables and none of the four above, which it could
  not use there.
- **The token is made here and goes nowhere else.** It is signed when the
  command starts, put in the command's environment, and removed — with
  every value a sandbox set — from the command's output and from every
  error before anything reaches the envelope or the trace.
- **Only `exec` takes a grant.** `attach`, `connect` and the other verbs
  refuse it, and so does `exec` with `stream`.
- **The grant must be this stack's, and for this workspace.** A rule cannot
  hand a command another stack's grant, or carry a grant to a workspace it
  was not minted for.
- **What a sandbox sets wins** over `env` and `secrets.env.*` of the same
  name. Two sandboxes that set one variable are refused.

| `workspace.error.code` | meaning |
|---|---|
| `grant_refused` | A sandbox was refused (the message names it, never why: the trace has that), or the grant does not name it, has ended, is another stack's, or was minted for another workspace. Or the node has no master key to sign with. The message says which. |
| `grant_unavailable` | This node opened no identity store |
| `bad_request` | `grant` is not a run grant's id; `sandbox` is not a name or a list of them, or is given without `grant`; `grant` names no sandbox on a provider whose commands cannot reach this chassis (nothing would be handed over); or `grant` is combined with `stream` or a verb that runs no command |

## `txco sandbox`

```sh
"$TXCO_BIN" sandbox github -- gh pr list
"$TXCO_BIN" sandbox github postgres -- ./migrate
```

`txco sandbox` asks the chassis to open each named sandbox, with the token
this command was started with, then becomes the program. It is for a
program that decides on its own when it needs what: an agent loop, a
script with many steps. It needs the `grant` personality on the chassis.

- **The `--` is required.** A program's name is as good a sandbox name as
  any.
- **The program gets what the sandboxes set and none of the grant's
  variables**, so it cannot open more.
- **Exit codes and signals are the program's own** once it starts.

| exit, before the program starts | meaning |
|---|---|
| `77` | A sandbox was refused |
| `69` | The chassis could not be asked, or could not decide |
| `65` | A value holds a NUL byte, which no variable can |
| `126`, `127` | The program could not be started, or was not found |
| `2` | The command line, or two sandboxes that set one variable |

### Whatever a command prints is in the trace

A command that prints a secret it was handed puts it in the envelope and
the trace. The chassis removes the grant's own token, and every value it
opened a sandbox with, from a command's output. It cannot know what else
the command read.

## What decides a request

Each secret a sandbox names is one request.

```text
 1  HARD CHECKS        the grant is genuine, live, current, and names the sandbox   no ──▶ refused
 2  THE PROPOSAL       the chassis's own answer
 3  THE _grant STACK   the tenant's rules may change it
 4  THE VERDICT        refuse, or charge the budget and hand over
```

| Check | Kind |
|---|---|
| The token is genuine and unexpired | Hard |
| The run grant is live, and the newest of its run | Hard |
| The tenant is live | Hard |
| The run grant names the sandbox | Hard |
| The secret exists | Hard |
| The run grant names the secret | Proposal |
| The principal holds a standing grant to release it | Proposal |
| The secret's pull policy admits this node | Proposal |
| Budget remains | Proposal |

- **A hard check is the chassis's alone.** No rule changes it.
- **The proposal is to allow when every proposal check passes.**
- **The tenant's `_grant` stack has the last word.** A rule may allow what
  the chassis proposed to refuse, or refuse what it proposed to allow.
  [The grant inlet](./protocols/grant.md) says how.
- **With no `_grant` rules, the proposal stands.**
- **Everything fails closed.** A rule that errors, takes too long or is
  refused admission is a refusal.

### The pull policy

A secret says for itself whether work may be handed it.

| `pull` | meaning |
|---|---|
| `none` | No work may be handed it. **The default.** A rule that names the secret still uses it as before. |
| `reviewed` | Work on a `reviewed` node may |
| `any` | Work on any node may |

```sh
txco auth tenant secrets policy DB_DSN --pull reviewed
txco auth tenant secrets list        # the PULL column
```

**A secret that was handed over cannot be recalled.** Revoking a grant
stops the next request. Only rotating the secret ends a past one. Mark a
secret `any` only when its loss is tolerable.

## Turning it on

The push form — `exec WITH grant, sandbox` — needs nothing turned on
beyond a workspace provider. For `txco sandbox`, add the `grant`
personality: the Unix socket a program on this machine reaches the chassis
on.

```sh
txco dev --allow-local-workspace --grant
```

- **The ops need nothing.** `grant/*` and `delegate/*` work on every node.
- **Listed and unable to listen is fatal at boot.** A node that was meant to
  answer and silently does not is worse than one that will not start.

## Which stack may grant

The [creator-stack rule](./users.md#which-stack-may-manage-a-principal)
covers both kinds: only the stack that manages a principal may grant to it
or mint for it, and a canary slot or a channel counts as its stack.

## Errors

Failures land as `<into>.error.{code, message}` and the run continues:

| code (`txco_grant_…` / `txco_delegate_…`) | meaning |
|---|---|
| `invalid_arg` | a bad or missing param; the message says which |
| `not_found` | no such principal or grant **in this tenant** |
| `not_owner` | another stack manages this principal, or minted this grant; the message names it |
| `user_disabled` | the principal is a disabled user |
| `too_many` | the principal already holds 1024 live grants |
| `no_sandbox` | `allow` names a sandbox this stack does not declare; the message names what it has |
| `exceeds_standing` | a sandbox names a secret the principal holds no standing grant for |
| `exceeds_parent` | a sandbox names a secret the parent does not, or the budget is more than the parent has left |
| `parent_not_live` | the parent grant has expired, ended or been revoked |
| `depth` | the grant would be narrowed more than 4 times |
| `stale_generation` | the run already has this `generation` or a later one |
| `no_tenant` · `no_stack` | the request has no tenant, or the op was not dispatched by a rule |
| `disabled` | this node could not open the identity database at boot |
| `store` | the database failed; the message has the detail |

## Flags

| flag | default | meaning |
|---|---|---|
| `--run-grant-ttl-default` | `600` | Seconds a run grant lasts when `mint` gives no `ttl` |
| `--run-grant-ttl-max` | `3600` | Ceiling on `ttl`. At most 86400. |
| `--run-grant-budget-default` | `50` | Requests a run grant may make when `mint` gives no `budget` |
| `--run-grant-budget-max` | `1000` | Ceiling on `budget`. At most 1000000. |
| `--grant-decide-timeout` | `5s` | How long the `_grant` stack may take to decide one request |
| `--grant-refusal-window` | `5s` | How long a refusal is answered from memory. `0` turns it off. |
| `--grant-socket` | under the system's temp dir | The socket of the `grant` personality. At most 100 bytes. |

**A refusal is remembered for a few seconds**, so a program that asks again
and again cannot run the `_grant` stack without end. A change that would
now allow the request takes up to `--grant-refusal-window` to be seen by a
run that was just refused. A release is never remembered.

## Where it lives

In the auth database, beside [users and credentials](./users.md#where-it-lives).
On a fleet every node sees the same grants, so a grant minted on one node
is honored, and revoked, on all of them. The sandboxes are stack files, in
the runtime database with the rest of the stack.

See `examples/grants-hello` for all of it in one stack.
