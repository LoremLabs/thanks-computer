<!-- nav: Capabilities -->

# Capabilities — what dispatched work may ask this chassis to do

_A stack dispatches work to a **node**: another chassis, running stacks of
its own, on a machine the work can use freely. The node holds none of your
keys. When the work needs something only this chassis should do — call a
model, write a record, send a message — it asks by name, and this chassis
decides and does it. That name is a capability._

```text
   PARENT (this chassis)                         NODE (a chassis the work runs on)
   ─────────────────────                         ──────────────────────────────────
   run grant ── token ──▶ exec in the workspace ──▶ the node's run
                                                        │
   POST /v1/cap/mail.send  ◀── cap://mail.send ─────────┘   the token, and the input
        │
   1  the grant is genuine, live, and names mail.send
   2  the tenant's _grant stack: allow, refuse, or hold
   3  the stack that DECLARES mail.send runs it          OPS/<stack>/CAPS/mail.send.yaml
        │
        └── {"ok":true,"output":…} ─────────────────▶ the rule's `into`
```

A [grant](./grants.md) says what work may be **handed** (a secret, into a
sandbox). A capability is the other thing a grant may name: something the
work may ask this chassis to **do**. Nothing is handed over, so a node that
is taken over holds nothing of yours; it can only ask, and every ask is
decided when it is made.

| To | Read |
|---|---|
| Say that a stack answers a capability | [Declaring one](#declaring-one) |
| Answer a call | [Answering a call](#answering-a-call) |
| Say what a run may call | [What a run may call](#what-a-run-may-call) |
| Call one from a node | [`cap://`](#cap) |
| See what a tenant offers | [The catalogue](#the-catalogue) |
| Decide each call | [What decides a call](#what-decides-a-call) |
| Run a node | [The node and its parent](#the-node-and-its-parent) |

## Declaring one

A stack says it answers a capability with one small file, named for the
capability, under the reserved `CAPS/` directory beside `SANDBOXES/` and
`OUTLETS/`:

```yaml
# OPS/crm/CAPS/crm.lookup.yaml
description: Look a customer up by email address.
input:
  email:
    description: the customer's address
    required: true
timeout: 30000
```

| | |
|---|---|
| **The name** | The file's stem: lowercase words joined by dots (`crm.lookup`, `mail.send`), each word `[a-z][a-z0-9_-]*`. It is what a node calls, what a sandbox and a standing grant name, and what the `_grant` stack sees. |
| **`description`** | Optional. What a caller reads to decide whether to call: a person, or a model choosing a tool. At most 2048 characters. |
| **`input`** | Optional. The fields a call carries: `<field>: {description, required}`. A field is `[a-z][a-z0-9_]*`; at most 32. It is published in [the catalogue](#the-catalogue); the chassis does not check a call against it — the stack that answers checks its own input. |
| **`timeout`** | Optional, milliseconds. It can only shorten how long the chassis waits for the answer (`--op-timeout-max` is the ceiling). |
| **`entry`** | Optional. See below. |
| **Anything else** | A deploy error. Only what the chassis reads is declared. |

- **The file's existence is the declaration.** Every key is optional; an
  empty file says "this stack answers this capability".
- **A call enters the stack at its start**, `<stack>/0`, like every other
  inlet's run. The stack's scopes run in order and their `WHEN`s pick the
  call up.
- **`entry` is for a stack that does other work from its start.** It names
  the scope of this stack a capability's run begins at, so the scopes before
  it never see the call:

  ```yaml
  # OPS/desk/CAPS/card.note.yaml — desk's first scopes serve its web pages
  entry: 7000
  ```

- **One stack answers a capability.** Activation refuses a name another
  active stack of the tenant already declares, a file that does not parse,
  and an `entry` that names no scope of the stack. `txco lint` and `txco
  apply` check the same things first.
- **It deploys with the stack**, like a sandbox or an outlet: `txco apply`
  uploads it, and deactivating the stack withdraws the capability.

## Answering a call

An allowed call is an ordinary run of the declaring stack, with `@src ==
"cap"` and the call under `@cap`:

```txcl
# OPS/crm/100/lookup.txcl
WHEN @src == "cap" && @cap.name == "crm.lookup" && @cap.input.email =~ /@/
  WITH sql  = "select name, plan from customers where email = $1",
       args = &array(@cap.input.email),
       into = "_row"
  EXEC "outlet://crm_read/query"
```

```txcl
# OPS/crm/110/answer.txcl
WHEN @src == "cap" && @cap.name == "crm.lookup" && ._row.rows.0.name =~ /./
  EMIT ._cap.output = &object("name", ._row.rows.0.name, "plan", ._row.rows.0.plan)
```

```txcl
# OPS/crm/110/missing.txcl
WHEN @src == "cap" && @cap.name == "crm.lookup" && ._row.rows.0.name !~ /./
  EMIT ._cap.error = &object("code", "no_such_customer", "message", "nobody has that address")
```

| fact | holds |
|---|---|
| `@cap.name` | The capability that was called |
| `@cap.input` | What the call carried: a JSON object, `{}` when it carried none |
| `@cap.run`, `@cap.generation` | The run the grant was minted for, and which minting of it |
| `@cap.grant` | The run grant's id |
| `@cap.principal.id`, `.kind` | Whom the work acts for |
| `@cap.stack`, `@cap.workspace` | The stack that minted the grant, and the workspace it was minted for |
| `@cap.caller.rid` | The request id of the node's run that called, so one piece of work can be followed across both chassis |

| the run leaves | the caller gets |
|---|---|
| `._cap.output` | That value, at the rule's `into` |
| `._cap.error = {code, message}` | A failure with that code. `._cap.error` wins when both are set. |
| neither | `txco_cap_unknown`: nothing answered |

- **Everything under `@cap` is the chassis's**, stamped from the run
  grant's own row after the token verified and the call was allowed. Only
  `@cap.input` came from the caller.
- **Treat the input as untrusted.** It was written on a machine you do not
  control, perhaps by a model. Check it here.
- **The run is an ordinary run**: admitted, traced, metered and bounded
  like any other. It reads the declaring stack's own outlets, secrets and
  store.

## What a run may call

Three things must agree before a call is allowed, each a narrowing of the
one before:

```yaml
# OPS/desk/SANDBOXES/workstation.yaml — what one run may call
description: a run on a workstation
capabilities:
  - crm.lookup
  - mail.send
```

```txcl
# once: the principal may ever call crm.lookup
WHEN ._setup.ok == true
  WITH principal = "service:research", kind = "capability", name = "crm.lookup"
  EXEC "txco://grant/put"
```

```txcl
# per run: this run may call what the workstation sandbox names
WHEN ._task.id =~ /./
  WITH principal = "service:research", run = ._task.id,
       allow = &array("workstation"), workspace = "bench", into = "_delegate"
  EXEC "txco://delegate/mint"
```

| | says |
|---|---|
| `CAPS/<name>.yaml` | Who answers the capability |
| A [standing grant](./grants.md#standing-grants), `kind = "capability"` | The principal may ever call it |
| A [sandbox](./grants.md#sandboxes)'s `capabilities:` | A run whose grant names this sandbox may call it |
| The [run grant](./grants.md#run-grants) | This run, until it ends, within its budget |

- **A sandbox may name capabilities, variables, or both.** One that names
  only capabilities hands the work nothing: opening it is what gives the
  command its token.
- **Each allowed call costs one request of the run grant's budget**, as a
  released secret does.
- **Ending the grant ends the calls.** A revoked, closed or expired grant
  is refused on the node's next call.

## `cap://`

On the node, a rule calls a capability the way it calls any op:

```txcl
WHEN ._draft.ready == true
  WITH input   = &object("email", ._draft.to),
       into    = "_customer",
       timeout = 30000
  EXEC "cap://crm.lookup"
```

| `WITH` | |
|---|---|
| `input` | What the capability is asked, as JSON. Default `{}`. |
| `into` | Where the answer's output lands. Default `_cap`. |
| `timeout` | The whole round trip, in milliseconds. |
| `name` | Replaces the name in the ref, as `WITH url` does for an `http` op: a rule that picks the capability from data writes `EXEC "cap://any.name" WITH name = ._call.name`. |

What comes back, beside `into`:

| | |
|---|---|
| `cap.name`, `cap.status`, `cap.ms` | The capability, the answer's status, the round trip in milliseconds |
| `cap.rid` | The parent's request id for the call: its trace of the decision and the run |
| `cap.error.{code, message, status}` | Set when the call did not get an answer. `into` is `{}` then. |

**Every failure is data**, so a rule can gate on it and the run goes on:

| `cap.error.code` | meaning |
|---|---|
| `txco_cap_denied` | Refused: this run arrived with no grant, the grant has ended, or the allowlist, the standing grant, the budget or a `_grant` rule said no. The parent's trace has the reason; the node is not told. |
| `txco_cap_held` | A `_grant` rule held the call for a person. It did not run. |
| `txco_cap_unknown` | Not a capability name, or nothing on the parent answered it |
| `txco_cap_unconfigured` | This chassis has no parent (`--parent-url`) |
| `txco_cap_timeout` | The parent did not answer in time |
| `txco_cap_unavailable` | The parent could not decide, or the run failed |
| `txco_cap_transport` | The parent could not be reached |
| _the stack's own code_ | The answering stack set `._cap.error`: its `code` and `message` arrive as written |

- **The rule never sees the token.** It arrived with the request that
  started this run, in one header; the web head moved it into the run's
  context before any rule ran, and `cap://` presents it. It is in no
  envelope and no trace.
- **A run that arrived without a grant can call nothing.**

## The catalogue

What a tenant's active stacks declare is its catalogue.

```sh
txco caps list
```

```text
NAME        ENTERS     TIMEOUT  INPUT   DESCRIPTION
card.note   desk/7000  -        text*   Note something on the card.
crm.lookup  crm/0      30000ms  email*  Look a customer up by email address.
```

A rule reads the same list, so a stack that offers capabilities to a model
— a tool list — carries no copy of it:

```txcl
# prefix is optional: it narrows the list by name
WHEN ._task.id =~ /./
  WITH into = "_caps", prefix = "crm."
  EXEC "txco://caps/list"
# → _caps = {count, items: [{name, stack, entry, stage, description,
#                            input: {<field>: {description, required}},
#                            params: [<field>, …], timeout}]}
```

- **`stage` is where a call enters**: `<stack>/0`, or `<stack>/<entry>`. A
  stack on the same chassis can run a capability by routing there; one on
  another chassis calls it by `cap://`.
- **A node has a catalogue too.** A package installed on a node may declare
  capabilities of its own, and the node's stacks list them the same way: a
  node's tools are what is installed on it.
- **The catalogue says what exists, not what a run may call.** That is the
  run grant's, and the `_grant` stack's.

`GET /v1/tenants/{tenant}/caps` on the admin plane is the same list.

## What decides a call

```text
 1  HARD CHECKS        the token is genuine and unexpired; the grant is live and current   no ──▶ refused
 2  THE PROPOSAL       the grant names the capability · a standing grant · budget remains
 3  THE _grant STACK   the tenant's rules may change it, or hold it
 4  THE VERDICT        refuse, or charge the budget and run the declaring stack
```

The `_grant` stack sees a call the way it sees a secret's release
([the grant inlet](./protocols/grant.md)), with `@grant.kind ==
"capability"`, the name in `@grant.name`, `@grant.via == "http"`, and the
call's input at `@grant.input`:

```txcl
# OPS/_grant/10/outside.txcl — mail to anyone outside the company waits for a person
WHEN @src == "grant" && @grant.kind == "capability" && @grant.name == "mail.send"
  && @grant.input.to !~ /@example\.com$/
  EMIT @grant.res.hold   = true,
       @grant.res.reason = "mail outside the company is approved by a person"
```

- **A hold is a refusal the caller can tell apart** (`txco_cap_held`). The
  chassis stores nothing: a rule that holds records what a person will need
  to decide, and your own stack runs the call later if they approve.
- **A refusal is remembered for a few seconds** (`--grant-refusal-window`),
  as it is for a secret.
- **Everything fails closed.**

[The capability inlet](./protocols/cap.md) has the request, the answers and
the `_cap` stack.

## The node and its parent

A node is a chassis started with its parent's address:

```sh
txco serve --parent-url https://desk.example.com
```

and work reaches it as an ordinary web request that carries the run grant's
token in one header. The dispatching rule starts a command in the node's
workspace with the grant, and the command makes the request:

```txcl
# on the parent: dispatch one run to the node, and hold the request open
WHEN ._delegate.id =~ /^rgr_/
  WITH workspace = "bench",
       grant     = ._delegate.id,
       sandbox   = "workstation",
       command   = "curl -sS --data-binary @- -H \"Txco-Run-Grant: $TXCO_RUN_GRANT\" http://127.0.0.1:8926/run",
       stdin     = ._task,
       into      = "_dispatch"
  EXEC "workspace://bench/exec"
```

- **The token travels in that one header.** The node's web head takes it
  out of the request before the envelope is built and keeps it in the run's
  context; every `cap://` call of that run presents it.
- **The parent is the operator's choice, never a rule's.** `--parent-url`
  is the base URL of the parent's web listener; a `cap://` call POSTs to
  `<parent-url>/v1/cap/<name>`. The tenant is the grant's, not the
  hostname's.
- **The node needs no credential of the parent's.** It is reached by an
  exec and answers by asking; what it may ask is the grant's, and the grant
  ends with the run.
- **An allowed call answers its headers at once** and its body when the
  capability's run ends, so a proxy in between does not cut a slow call.
  See [the inlet](./protocols/cap.md#the-answers).

| flag | default | meaning |
|---|---|---|
| `--parent-url` | none | On a node: the parent's base URL. Without it every `cap://` call answers `txco_cap_unconfigured`. |
| `--op-timeout-max` | | On the parent: the ceiling on one capability's run |
| `--grant-decide-timeout` | `5s` | How long the `_grant` stack may take to decide one call |
