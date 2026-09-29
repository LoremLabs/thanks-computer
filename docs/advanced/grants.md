<!-- nav: Grants -->

# Grants — what a principal, and one piece of its work, may ask for

_A stack dispatches work somewhere else: a command in a
[workspace](../workspaces.md), acting for a [principal](./users.md#principals).
That work may need a secret, or to call something back. Grants say what it
may ask this chassis for._

> **In this release** grants are written, read and ended. Nothing asks the
> chassis for anything with one yet: handing a grant to a command, and
> reading a secret with it, arrive in the next release.

There are two kinds, and the second can only narrow the first.

| | Standing grant | Run grant |
|---|---|---|
| **Says** | This principal may ever ask for this | This piece of work may ask for these, now |
| **Lasts** | Until revoked | Minutes: it expires, and ends with the work |
| **Written by** | `txco://grant/put` | `txco://rungrant/mint` |
| **Holds** | One principal, one resource, its verbs | An allowlist, a budget, an expiry, a workspace |

A grant names one of two kinds of resource:

| kind | named by | verb | the principal may |
|---|---|---|---|
| `capability` | lowercase words joined by dots: `crm.lookup` | `invoke` | call it |
| `secret` | a secret's name: `CRM_KEY` | `release` | be handed the secret itself |

A name is exact. There are no wildcards and no families: `crm.lookup` does
not grant `crm.update`.

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
run grant names what one piece of work may ask for, and the secret's own
pull policy and the tenant's rules decide each request.

## Run grants

Mint one when you dispatch the work:

```txcl
# 3400: this task may read one secret and make one kind of call
WHEN ._task.id != ""
  EXEC "txco://rungrant/mint"
    WITH principal = "service:research",
         run       = ._task.id,
         allow     = ["secret:CRM_KEY", "crm.lookup"],
         workspace = "bench",
         ttl       = 900,
         budget    = 20
# → _rungrant = {id, principal, run, generation, workspace, workspace_id,
#                node_class, allow, budget, spent, depth, minted_by,
#                issued_at, expires_at, live}
```

| op | WITH | result |
|---|---|---|
| `rungrant/mint` | `principal`, `run`, `allow`, and the optional params below | the grant |
| `rungrant/get` | `id` | the grant, what it has `spent`, and whether it is `live` |
| `rungrant/revoke` | `id` | the grant, and `revoked` |
| `rungrant/close` | `id`, `reason?` (default `done`) | the grant, and `closed` |

All four answer at `into` (default `_rungrant`). Only the stack that minted
a grant may read, revoke or close it.

| mint param | meaning | default |
|---|---|---|
| `run` | The name of the work: a task id, a job id. Letters, digits and `. _ @ + : / -`. | required |
| `allow` | What the work may ask for: a capability by name, a secret as `secret:<NAME>`. 1 to 64 names. | required |
| `workspace` | A workspace of the calling stack, by name: where the work is sent. It need not exist yet. | none |
| `node_class` | `reviewed` or `unreviewed`: what is allowed to run there | `unreviewed` |
| `ttl` | Seconds the grant lasts | `--run-grant-ttl-default` |
| `budget` | Requests the work may make | `--run-grant-budget-default` |
| `generation` | A counter of your own, such as a lease. It must be later than any the run has had. | the next one |
| `parent` | The id of a run grant to narrow from | none |

**There is no token in the result.** The token that travels with the work
is signed by the chassis when it hands the work over, and never enters an
envelope or a trace. A rule holds the grant's `id`, which names the grant
and authorizes nothing.

### A run grant only narrows

| Rule | Refused as |
|---|---|
| `allow` names only what the principal holds a standing grant for | `exceeds_standing`; the message names what is missing |
| A grant narrowed from a `parent` names only what the parent names | `exceeds_parent` |
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
| `rungrant/close`: the work ended | `closed_at`, and your `close_reason` |
| `rungrant/revoke` | `revoked_at` |
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
runs on a machine. A secret's pull policy is checked against it.

## Which stack may grant

The [creator-stack rule](./users.md#which-stack-may-manage-a-principal)
covers both kinds: only the stack that manages a principal may grant to it
or mint for it, and a canary slot or a channel counts as its stack.

## Errors

Failures land as `<into>.error.{code, message}` and the run continues:

| code (`txco_grant_…` / `txco_rungrant_…`) | meaning |
|---|---|
| `invalid_arg` | a bad or missing param; the message says which |
| `not_found` | no such principal or grant **in this tenant** |
| `not_owner` | another stack manages this principal, or minted this grant; the message names it |
| `user_disabled` | the principal is a disabled user |
| `too_many` | the principal already holds 1024 live grants |
| `exceeds_standing` | `allow` names something the principal holds no standing grant for |
| `exceeds_parent` | `allow` names something the parent does not, or the budget is more than the parent has left |
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

## Where it lives

In the auth database, beside [users and credentials](./users.md#where-it-lives).
On a fleet every node sees the same grants, so a grant minted on one node
is honored, and revoked, on all of them.
