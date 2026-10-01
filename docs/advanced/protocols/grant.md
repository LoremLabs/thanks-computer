<!-- nav: Grant inlet -->

# The grant inlet — your rules decide each request

_Work you dispatched opens a sandbox, and each secret the sandbox names is
a request. Before the chassis answers one, it presents the request to your
tenant's `_grant` stack, with the answer it proposes. Your rules may change
that answer either way._

[Grants](../grants.md) says what a grant and a sandbox are and how a
command uses them. This page is about the stack that decides.

## One request is one run

```text
OPS/_grant/
  0/journal.txcl      keep a record
  10/policy.txcl      decide
```

A request enters `_grant/0` as an envelope with `@src == "grant"`, the way
a model call enters `_llm`. It is an ordinary run: it is admitted, traced
and metered like any other. A sandbox that names three secrets is three
runs.

- **A tenant with no `_grant` stack runs no rule**, and the chassis's
  proposal stands.
- **A request that fails a hard check never gets here.** A forged token, an
  expired one, a revoked grant, or a sandbox the grant does not name is
  refused before any rule runs.
- **One refusal refuses the sandbox.** The program is handed nothing, and
  what was already allowed stays charged.

## What a rule sees

Everything is under `@grant`, stamped by the chassis from the run grant's
own row. It is read-only, except `@grant.res`.

| fact | holds |
|---|---|
| `@grant.phase` | `request` |
| `@grant.kind`, `@grant.name`, `@grant.verb` | What was asked for: `secret`, its name, `release` — or, for a [capability](../capabilities.md) call, `capability`, its name, `invoke` |
| `@grant.sandbox`, `@grant.env` | The sandbox being opened, and the variable this secret fills |
| `@grant.principal.id`, `.kind` | Whom the work acts for |
| `@grant.run`, `@grant.generation` | The run, and which minting of it |
| `@grant.grant`, `@grant.depth` | The run grant's id, and how many times it was narrowed |
| `@grant.stack`, `@grant.workspace` | The stack that minted it, and the workspace it was minted for |
| `@grant.node.class` | `reviewed` or `unreviewed` |
| `@grant.via` | Who opened the sandbox: `exec` (the rule that started the command) or `launcher` (`txco sandbox`, the program itself). `http` for a capability call. |
| `@grant.input` | A capability call's input, as the caller sent it: data for a rule to decide on. Absent for a secret. |
| `@grant.secret.pull` | The secret's pull policy |
| `@grant.secret.version`, `.scope` | Its version, and `tenant` or `stack` |
| `@grant.checks.exists` | Whether there is such a secret. Always `true` for a capability: whether anything answers it is found when it runs. |
| `@grant.checks.allowlist`, `.standing`, `.pull`, `.budget` | Each check of the proposal, `true` or `false` |
| `@grant.budget.calls`, `.spent` | The run's budget, and what it has spent |
| `@grant.proposed.allow` | The chassis's answer |
| `@grant.proposed.reason` | Why not, when it is `false`: `not_found`, `allowlist`, `standing`, `pull` or `budget` |

The envelope never holds the secret's value or the grant's token.

## The verdict

```txcl
# allow one principal a secret its pull policy closed, through one sandbox
WHEN @src == "grant"
  && @grant.sandbox == "deploy"
  && @grant.name == "DEPLOY_KEY"
  && @grant.principal.id == "service:release"
  && @grant.checks.allowlist == true
  && @grant.checks.standing == true
  EMIT @grant.res.allow  = true,
       @grant.res.reason = "release may deploy"
```

| a rule writes | the request is |
|---|---|
| nothing | Decided by the proposal |
| `@grant.res.allow = true` | Allowed |
| `@grant.res.allow = false` | Refused |
| `@grant.res.hold = true` | Refused, as `held`. A capability's caller is told so distinctly (`409 held`, `txco_cap_held` on the node). The chassis stores nothing: a rule that holds records what a person will need, and your stack runs the call later if they approve. |
| `@grant.res.reason = "…"` | Unchanged. The reason is kept in the log. |

- **`allow` is a boolean.** `"true"` or `1` is a rule that meant to decide
  and did not, and the request is refused.
- **A hold beats an allow.**
- **A rule cannot release what is not there.** A request for a secret that
  does not exist is presented, so it is traced, and then refused whatever
  a rule says.
- **A rule that allows is trusted with the budget too.** If the chassis
  proposed to refuse for want of budget and a rule allowed the request, the
  call is charged past the budget.
- **Check what you mean to keep.** A rule that writes `allow = true` on the
  name alone also allows a run whose grant does not name the secret.
  Include `@grant.checks.allowlist` and `@grant.checks.standing` unless you
  mean to overrule them.

## What refuses, whatever was proposed

| the run | the request is |
|---|---|
| Errors | Refused |
| Takes longer than `--grant-decide-timeout` | Refused |
| Is refused admission: rate, concurrency, a suspended tenant | Refused |
| Streams a response | Refused |

## Keeping a record

The trace of the `_grant` run is the record of a request: `txco trace`.
The chassis keeps no journal of its own. To keep one you can read back,
write it:

```txcl
# OPS/_grant/0/journal.txcl
WHEN @src == "grant" && @grant.phase == "request"
  EXEC "txco://notebook/append"
    WITH notebook = &concat("grants/", @grant.run),
         type     = "grant.request",
         data     = &object("sandbox", @grant.sandbox, "env", @grant.env,
                            "secret", @grant.name, "version", @grant.secret.version,
                            "principal", @grant.principal.id, "proposed", @grant.proposed)
```

After an incident, that record answers what rotation needs: which versions
of which secrets reached which runs.

Gate every rule on `@grant.phase == "request"`. Later phases will enter the
same stack.
