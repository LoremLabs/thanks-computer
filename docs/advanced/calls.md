<!-- nav: Calls -->

# Calls — a stack runs another stack and gets an answer

_A call runs another stack of the same tenant **in process, inside the same
request**, on a fresh envelope, and brings back one result object. It
isolates **data**, not **authority**: the callee runs with the request's
tenant, principal, secrets and run grant. A call is not a security boundary.
Not calling a stack protects nothing that stack can reach, and calling it
grants nothing a jump would not._

```text
  ONE REQUEST, shared by all its frames: tenant, principal, rid, opstack
  snapshot, fuel and TTL, deadline, trace, secrets, run grant

  FRAME 0: app (the inlet's)                    FRAME 1: app/_orient (a call)
  +------------------------------------+        +------------------------------------+
  | 100  load the job                  |        | boundary: app/_orient only         |
  |                                    |        |                                    |
  | 200  EXEC "call://app/_orient"     |-input->| 100  WHEN @src == "call"           |
  |      WITH input = ...,             |        |      reads @call.input             |
  |           into  = "_orient"        |        | 200  ...                           |
  |        :                           |        | 300  EMIT ._call.output = ...      |
  |        :  the op waits             |        |                                    |
  |        v                           |        | ends on: @halt, no scope left,     |
  |      ._orient = { ok, output,      |<-result| an error, or the deadline          |
  |         error, stack, version,     |        |                                    |
  |         ms }                       |        | never runs app's scopes;           |
  |                                    |        | a jump out ends the call           |
  | 300  WHEN ._orient.ok == true ...  |        |                                    |
  +------------------------------------+        +------------------------------------+
```

| To | Read |
|---|---|
| Call a stack | [Calling](#calling) |
| Answer a call | [Answering](#answering) |
| Read the result | [The result](#the-result) |
| Declare what a stack takes and answers | [Declaring an entry](#declaring-an-entry) |
| Know what a call may and may not do | [The boundary](#the-boundary), [What a call refuses](#what-a-call-refuses) |
| Bound a call | [Deadline and cancellation](#deadline-and-cancellation), [Depth and fuel](#depth-and-fuel) |
| See it in a trace | [In the trace](#in-the-trace) |
| Choose between a call and a jump | [A call, a jump, a capability](#a-call-a-jump-a-capability) |

## Calling

```txcl
WHEN ._job.id =~ /./
  WITH input   = &object("job", ._job, "text", ._msg.text),
       into    = "_orient",
       timeout = 20000
  EXEC "call://app/_orient"
```

| | |
|---|---|
| **`call://<stack>`** | Enter the stack at its first scope. |
| **`call://<stack>/<scope>`** | Enter it at a floor, as a jump would. The last segment is the scope when it is all digits. |
| **`WITH input`** | What the callee is given, as JSON (default `{}`). It is the only part of the caller's envelope the callee sees. |
| **`WITH into`** | Where the result object lands (default `_call`). |
| **`WITH timeout`** | The call's own limit, in ms or as a duration (default `--call-default-timeout`, 60 s). See [the deadline](#deadline-and-cancellation). |
| **`WITH stack`** | Replaces the ref's stack from data, as `WITH name` does for `cap://`. A value that is not a stack name answers `txco_call_target`. |

A literal target is checked at `txco apply`, like `goto://`. Whether the
callee **exists** is not: it deploys separately, and a call to a stack the
chassis does not have answers `txco_call_no_stack` as data. `txco apply`
warns when the callee is not in the same apply.

## Answering

A callee starts from a fresh envelope the chassis writes:

| Field | Value |
|---|---|
| `@src` | `"call"` |
| `@call.input` | the resolved `WITH input`, read-only like all of `_txc` |
| `@call.caller` | `{stack, stage, op}` of the calling rule |
| `@call.depth` | 1 for a call from a request, one more per nested call |
| `@tenant`, `@rid`, `@principal`, `_ts` | the request's |
| `@stack` | the callee's own stack, so its kv, notebook, read-file and dataset defaults are its own |

Rules cannot write `_txc`, so a callee cannot forge its input or its
caller. Gate the entry scope on the source, as a capability's gates on
`@cap.name`, so no other route into the stack can pose as a call:

```txcl
# OPS/app/_orient/100_COMPILE/compile.txcl
WHEN @src == "call" AND @call.input.job.id =~ /./
  ...
  EMIT ._call.output = &object("prompt", ._prompt, "sources", ._sources)
```

The callee answers by writing, at the top level of its own envelope:

| | |
|---|---|
| **`._call.output`** | Any JSON. What the caller gets as `output`. |
| **`._call.error`** | `{code, message}`, or a string (the message). What the caller gets as `error`, with `ok: false`. |

Nothing else of the callee's envelope comes back: not its working fields,
not its `@web.res`, not its `_txc`. The callee ends on `@halt`, when it has
no scope left, on an error, or on the deadline; the chassis then returns to
the caller in every case. A `@halt` ends the callee, never the request.

A callee that makes a call of its own with the default `into` leaves that
inner result at its `_call`, which is also what its caller reads back; a
callee that wants to answer for itself after such a call overwrites
`._call.output` (and clears `._call.error` with `@delete` if it handled the
inner failure).

## Declaring an entry

A stack that is called from several places, or by rules someone else
maintains, declares its entries: one small file per entry under the
reserved `CALLS/` directory, beside `CAPS/`:

```yaml
# OPS/app/_orient/CALLS/orient.yaml
description: Compile the context needed for a job.
entry: 100
timeout: 20000
input:
  job:
    description: the job record
    required: true
  text:
    description: the message to answer
output:
  prompt:
    description: the compiled prompt
    required: true
  sources:
    description: the documents it drew on
```

| | |
|---|---|
| **The name** | The file's stem, `[a-z][a-z0-9_-]*`. An entry is the stack's own: two stacks may each have an `orient`. |
| **`entry`** | The scope the call enters at (default 0, the stack's start). It must name a scope the stack has; `txco apply` refuses one that doesn't. |
| **`timeout`** | Milliseconds. It can only shorten the call's own timeout. |
| **`input`**, **`output`** | The fields the call carries and the answer carries: `<field>: {description, required}`, each `[a-z][a-z0-9_]*`, at most 32. Not a schema: the chassis checks names and `required`, nothing about shape. |

The shape is the [capability declaration](./capabilities.md#declaring-one)'s,
so a stack that answers callers inside the tenant and outside it writes the
same contract twice under two names.

A rule names the entry when it calls:

```txcl
WITH input = &object("job", ._job, "text", ._msg.text), entry = "orient", into = "_orient"
EXEC "call://app/_orient"
```

- The call enters at the declared scope, and `.entry` names the entry
  to the callee. Naming an entry and a scope together (`call://app/_orient/100`
  with `WITH entry`) is `txco_call_target`.
- **A missing required input fails before the callee runs:**
  `txco_call_invalid_input`, naming the fields.
- **A missing required output fails after it ends:**
  `txco_call_invalid_output`, with `output` still carrying what the callee
  did write.
- An entry the stack does not declare is `txco_call_no_entry`. `txco apply`
  warns when a literal `WITH entry` names one its callee, in the same apply,
  lacks.
- A call that names no entry is checked for nothing, as before.

`txco calls list` and `txco://calls/list` show every entry the tenant's
active stacks declare, with what each takes and answers, so a stack that
composes others reads the contracts instead of carrying a copy.

`examples/call-hello` is all of this in two stacks: a declared entry, a
capability answered here, both called from one scope, and a refusal
answered as data.

## The result

When the call ends, the caller finds **one object under `into`, and nothing
else**:

```json
"_orient": {
  "ok": true,
  "output": { "prompt": "…", "sources": ["…"] },
  "error": null,
  "stack": "app/_orient",
  "version": 12,
  "ms": 41
}
```

| | |
|---|---|
| **`ok`** | The chassis's verdict: the callee ended normally and wrote no `._call.error`. A callee that wrote neither answers `ok: true, output: {}` — a call made for its effects is normal. |
| **`output`** | The callee's `._call.output`, or `{}`. Still carried when `ok` is false, with whatever the callee did write. |
| **`error`** | `null`, the callee's `._call.error`, or the engine's own (below). |
| **`stack`**, **`version`** | Which stack answered, and its active version number in the request's pinned snapshot (0 when it cannot be told). |
| **`ms`** | The call's wall clock. |

Since nothing outside `into` is written, two calls in one scope cannot
collide. The caller gates as it does on an outlet:

```txcl
WHEN ._orient.ok == true   EMIT ._prompt = ._orient.output.prompt
WHEN ._orient.ok == false  EMIT ._fallback = true, ._why = ._orient.error.code
```

The engine's own error codes:

| Code | When |
|---|---|
| `txco_call_no_stack` | No stack of that name in this tenant. |
| `txco_call_target` | `WITH stack` is not a stack name, `WITH entry` is not an entry name, or an entry was named beside a scope. |
| `txco_call_no_entry` | The callee declares no such entry ([declaring an entry](#declaring-an-entry)). |
| `txco_call_invalid_input`, `txco_call_invalid_output` | The call lacks an input the entry requires (checked before the callee runs), or the answer lacks an output it promises (checked after; `output` carries what was written). |
| `txco_call_boundary` | The callee jumped to another stack ([the boundary](#the-boundary)). |
| `txco_call_unsupported` | The callee tried to suspend or stream ([what a call refuses](#what-a-call-refuses)). |
| `txco_call_timeout`, `txco_call_cancelled` | The deadline, or the request was cancelled; `stopped` and `running` say what the callee's ops did ([deadline](#deadline-and-cancellation)). |
| `txco_call_depth` | Calls nested past the limit ([depth](#depth-and-fuel)). |
| `txco_fuel_exhausted`, `txcl_scope_ttl_exhausted`, `txco_allowance_exhausted` | The request's budget ran out inside the callee; the request ends at its next charge. |
| `txco_call_failed` | The callee's run failed without an answer (a message says how). |
| `txco_call_error` | The callee wrote a `._call.error` with no code. |

## The boundary

**A call runs the named stack, and only it.**

- **Lookups never peel.** Outside a call, a slash-named stack is an overlay:
  when `app/onboard` has nothing at or above the next scope, the lookup falls
  back to `app` at the same scope and runs on there. Inside a call the lookup
  resolves the callee's own stack exactly, and when it has no scope left the
  call ends. A `_`-named child (`app/_onboard`) and a slot (`app/onboard`)
  are called alike.
- **Jumps stay inside.** A bare-scope jump (`EMIT @goto = "300"`) resolves in
  the callee as it does anywhere. A jump to another stack — `@goto`,
  `goto://`, an unschemed `EXEC "<stack>/<scope>"`, `txco://route` — ends
  the call with `txco_call_boundary`. To use another stack from inside a
  call, call it.

| Mechanism | Runs the parent's scopes too? |
|---|---|
| `goto://app/onboard/0` (and `@goto`, unschemed `EXEC`) | Yes: the overlay, unchanged |
| `call://app/onboard` | No |
| `call://app/_onboard` | No |

With `app = {100, 200}` and `app/onboard = {100, 150}`, from `app/100`: a
jump to `app/onboard/0` runs onboard's 100 and 150 and then app's 200, as
part of onboard, and nothing comes back; a call of `app/onboard` runs 100
and 150 only and returns, and the caller's next scope runs with the result
in hand.

## What a call refuses

A call cannot suspend and cannot stream; the chassis's continuation model
is tail-call only, so nothing could return into the caller's op afterwards.
Inside a call these end it with `txco_call_unsupported`, decided before
anything is dispatched, the same on every chassis whether or not it has a
continuation store:

- `WITH mode = "async"`, with or without `join_at_scope`;
- `WITH mode = "continuable"`;
- `@web.res.body` written in a scope that is not the callee's last (the
  explicit streaming idiom);
- `workspace://… WITH stream = true` refuses itself, as it does on any run
  with no client to stream to.

What a callee **may** use: every synchronous op, `LOOP` and `LOOP EVERY`,
`txco://schedule`, outlets, kv, `ai://`, websocket send and reply, and
calls of its own.

## Deadline and cancellation

The call's `timeout` is what it **asks** for. Its deadline is the least of:

```
the caller's remaining deadline
the call's timeout (WITH timeout, default --call-default-timeout = 60 s)
the chassis maximum (--op-timeout-max; a call over it is clamped, not dropped)
```

A callee can never extend its caller's lifetime.

**On cancellation, the call returns once the callee has stopped.** The
chassis cancels the callee's context and waits up to `--call-stop-grace`
(default 2 s) for the callee's ops to return, then answers
`txco_call_timeout` (or `txco_call_cancelled`) with:

```json
"error": { "code": "txco_call_timeout", "message": "app/_orient ran past its deadline",
           "stopped": false, "running": ["app/_orient/300/fetch"] }
```

Ops that honour their context — http, `ai://`, outlets, kv — stop well
inside the grace, and `stopped` is `true`. An op that ignores cancellation
cannot be stopped by the chassis: a write already handed to a database, a
`workspace://` command already running, mail already submitted. If such an
op outlives the grace, `stopped: false` says so and `running` names it; its
side effects may land after the call has returned.

## Depth and fuel

- **Depth.** Calls nest to **8**; `--call-depth-max` can lower that, never
  raise it. Over the limit, the result is `txco_call_depth`. Recursion is
  allowed within the limit.
- **Fuel and TTL** are the request's: the call pays the `EXEC` dispatch
  (25), and the callee's scopes and ops charge the same ledger at the usual
  rates ([fuel](./fuel.md)). Exhaustion inside the callee ends the callee
  with the exhaustion as its error, and the request at its next charge.
- **The transition history is the call's own.** Each call starts with an
  empty one, so a sub-flow called twice in one request pays no repeat
  penalty; a tight loop inside one call still does.

## In the trace

The callee's stages and steps append to the caller's request trace: it is
the same request. Two events bracket them, `call.enter` and `call.return`,
carrying the stack, **the callee's resolved version**, the depth, the wall
clock and the outcome, so a trace always says which code answered. A jump
out of the callee records `call.boundary` before the call ends. In
OpenTelemetry the callee's spans are children of the calling op's
`exec-call://…` span.

Redaction hints of every stack a request visits apply to the whole trace,
so a callee's hints also cover the caller's later steps.

## Capabilities answered here

The same engine has a second way in. `EXEC "cap://<name>"` on a chassis
where a stack of the request's tenant **declares** `<name>` (`CAPS/`,
[capabilities](./capabilities.md#answered-here)) runs that stack here, in
process, as a frame on this request — not up to a parent, and not through
the bus. The two ways in differ only in what they stamp and how they shape
the answer:

| | `call://` | `cap://`, declared here |
|---|---|---|
| Names | a stack, by place | a capability, by name; the catalogue says which stack |
| Enters at | the ref's scope (default 0) | the declaration's `entry` |
| The callee sees | `@src == "call"`, `@call.input`, `@call.caller`, `@call.depth` | `@src == "cap"`, `@cap.name`, `@cap.input`, `@cap.caller`, `@cap.depth`, `@cap.impl` |
| The callee answers at | `._call.output` / `._call.error` | `._cap.output` / `._cap.error`, as it would a call from a node |
| The caller gets | one result object under `into` | cap://'s shape: the output under `into`, `cap.name`, `cap.status`, `cap.ms`, failures at `cap.error` |
| Bounded by | `WITH timeout` | `WITH timeout`, and the declared `timeout` when shorter |
| No answer | `ok: true, output: {}` | `txco_cap_unknown` (the inlet's contract: no rule answered) |

Everything else is the same: the boundary, the refusals, the deadline and
the wait on cancellation, the depth, the shared budget, the per-frame
history, the trace events (marked `via: cap`). A capability may call a
capability, or a stack, and the depth counts across both.

## A call, a jump, a capability

| | Moves the stage | Moves the identity (`@stack`) | Returns | Runs the parent's scopes | Isolation | Authority |
|---|---|---|---|---|---|---|
| `@goto`, `goto://`, `EXEC "<stack>/<scope>"` | yes | no | no | yes (overlay) | none | the request's |
| `txco://route` | yes | yes | no | yes (overlay) | none | the request's |
| `call://` | for the callee | for the callee | **yes** | **no** | envelope only | the request's |
| `cap://`, declared here | for the callee | for the callee | **yes** | **no** | envelope only | the request's ([above](#capabilities-answered-here)) |
| `cap://` (up to a parent) | another chassis | — | yes | — | envelope | the run grant, decided by `_grant` ([capabilities](./capabilities.md)) |

Use a jump to hand a request over for good (the boot → service handoff, a
channel's entry). Use a call for a sub-flow whose answer you need: compiling
a context, validating a record, a step several stacks share. Use a
capability when the work runs on a node and must ask this chassis to do
something under a grant.

**Rolling out.** A chassis that predates calls does not refuse `call://`
cleanly: `call://x/100` matches the unschemed stage pattern and is run as a
jump to a stack literally named `call://x`, which ends the request there.
Do not push a stack that uses `call://` until the fleet runs a release that
has it.
