<!-- nav: Capability inlet -->

# The capability inlet — a node asks, your stack answers

_Work you dispatched to a node calls back for something only this chassis
should do. The call arrives as one HTTP request carrying the run's grant.
The chassis decides it, runs the stack that declares the capability, and
answers with what that stack left._

[Capabilities](../capabilities.md) says what a capability is, how a stack
declares one and how a node calls it. This page is the wire and the run.

## The request

```http
POST /v1/cap/crm.lookup HTTP/1.1
Host: desk.example.com
Txco-Run-Grant: rg1.…
Txco-Caller-Rid: CfbLw96hdZm715moMuCqf
Content-Type: application/json

{"input": {"email": "ada@example.com"}}
```

| | |
|---|---|
| **The path** | `/v1/cap/<name>` on the web listener, ahead of every hostname route. The tenant is the grant's, not the host's. |
| **`Txco-Run-Grant`** | The run grant's token: the whole authorization. No session, no basic auth. |
| **`Txco-Caller-Rid`** | Optional: the calling run's request id on the node, stamped as `@cap.caller.rid`. |
| **The body** | A JSON object; `input` is what the capability is asked. An empty body is an empty input. At most `--op-payload-max`. |

A node's `cap://<name>` op makes this request. Anything else that holds a
live run grant's token may make it too: the token is what is checked.

## One call is two runs

```text
 _grant/0        decide it          @src == "grant", @grant.kind == "capability"
 <stack>/<n>     do it              @src == "cap",   @cap.name, @cap.input
```

- **The decision** is a run of the tenant's `_grant` stack, as for a
  secret ([the grant inlet](./grant.md)): the chassis proposes, a rule may
  allow, refuse or hold. A call that fails a hard check — a forged or
  expired token, an ended grant — reaches no rule.
- **The work** is a run of the stack that declares the capability
  (`CAPS/<name>.yaml`), entered at its start, `<stack>/0`, or at the scope
  its declaration's `entry` names.
- **Both are ordinary runs**: admitted, traced and metered. The work run's
  request id is the answer's `X-Request-ID`; the node records it as
  `cap.rid`.

## The envelope

Everything under `@cap` is stamped by the chassis from the run grant's own
row, after the token verified and the call was allowed. It is read-only.
Only `@cap.input` is the caller's.

| fact | holds |
|---|---|
| `@cap.name` | The capability |
| `@cap.input` | The call's input, a JSON object |
| `@cap.run`, `@cap.generation` | The run, and which minting of it |
| `@cap.grant` | The run grant's id |
| `@cap.principal.id`, `.kind` | Whom the work acts for |
| `@cap.stack`, `@cap.workspace` | The stack that minted the grant, and the workspace it was minted for |
| `@cap.trace` | The trace the grant was minted in |
| `@cap.caller.rid` | The node's request id, when the caller sent one |
| `@cap.impl.stack`, `@cap.impl.to` | The declaring stack, and where the run entered |

The envelope never holds the grant's token.

The stack answers at the top level of the envelope:

| the run leaves | the answer is |
|---|---|
| `._cap.output` | `{"ok": true, "output": <that value>}` |
| `._cap.error = {code, message}` | `{"ok": false, "error": {code, message, status: 422}}`. It wins over an output. |
| neither | `{"ok": false, "error": {"code": "no_capability", "status": 404}}` |

## The answers

Always JSON. **The decision is answered with its own status**, before
anything runs:

| status | body | when |
|---|---|---|
| `401` | `{"ok":false,"error":{"code":"unauthorized"}}` | No token, or not a live grant's |
| `403` | `{"ok":false,"error":{"code":"denied"}}` | Refused: the allowlist, the standing grant, the budget, a rule |
| `409` | `{"ok":false,"error":{"code":"held"}}` | A rule held it for a person |
| `404` | `{"ok":false,"error":{"code":"no_capability"}}` | Not a capability name |
| `503` | `{"ok":false,"error":{"code":"unavailable"}}` | The chassis could not decide, or could not read the declaration |

A refusal says only that it was refused. The trace of the `_grant` run has
the reason.

**An allowed call answers `200` at once** — headers sent before the
capability's run starts — and the body when the run ends. Until then the
chassis writes one space every ten seconds. A model call can take a minute,
and a proxy on the way that waits a fixed time for response headers, or
drops an idle connection, would otherwise cut it. So what the run came to
is in the body, with the status it would have carried:

| body | when |
|---|---|
| `{"ok":true,"output":…}` | The stack answered |
| `{"ok":false,"error":{"code":…,"message":…,"status":422}}` | The stack's own error |
| `{"ok":false,"error":{"code":"no_capability","status":404}}` | No rule answered |
| `{"ok":false,"error":{"code":"timeout","status":504}}` | The run passed its ceiling: the declaration's `timeout`, or `--op-timeout-max` |
| `{"ok":false,"error":{"code":"unavailable","status":503}}` | The run failed, or the tenant was refused admission |

A client reads `error.status` as the status. The leading whitespace is
valid JSON. The node's `cap://` does both, and maps the codes to its own
grammar (`txco_cap_denied`, `txco_cap_held`, …;
[`cap://`](../capabilities.md#cap)).

## The `_cap` stack

A capability no active stack declares is not refused outright: the call
goes to the tenant's `_cap` stack, at `_cap/0`, the way a cron tick goes to
`_cron/0`. It is the place for a rule that answers by pattern, or routes a
family of names somewhere:

```txcl
# OPS/_cap/100/legacy.txcl — every report.* capability is answered by one stack
WHEN @src == "cap" && @cap.name =~ /^report\./
  SET @route.stack = "reports",
      @route.to    = "reports/0"
  EXEC "txco://route"
```

- **A declaration wins.** `_cap` sees only the names nothing declares.
- **A tenant with no `_cap` stack answers nothing** for those names, and
  the caller gets `no_capability`.
- **Prefer a declaration.** A declared capability is in
  [the catalogue](../capabilities.md#the-catalogue), is checked at deploy,
  and names its one answering stack; a pattern in `_cap` is none of those.
- **The decision came first either way.** `_cap` runs only what `_grant`
  allowed, for a name the run grant and a standing grant both carry.

## What ends a run, whatever it was doing

| the run | the answer is |
|---|---|
| Takes longer than its ceiling | `timeout` (504, in the body) |
| Errors | `unavailable` (503, in the body) |
| Is refused admission: rate, concurrency, a suspended tenant | `unavailable` (503, in the body) |
| The caller goes away | The run is cancelled |

## Keeping a record

The two traces — the `_grant` run and the capability's run — are the
record of a call: `txco trace`. To keep one you can read back, write it
where the decision is made, as [the grant inlet](./grant.md#keeping-a-record)
shows; `@grant.input` is there for a capability call, so the record can say
what was asked as well as what was decided.
