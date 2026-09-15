<!-- nav: Deferred join -->

# Deferred join — keep the flow moving while an async op runs

_A `mode = "async"` op normally suspends the flow at its own scope: nothing
after it runs until the worker calls back. `join_at_scope` splits that into
two moments — the op is **dispatched** at its own scope and **joins** at a
later scope in the same stack. The scopes in between run straight away; the
op's result merges into the document just before the join scope's ops run._

> **The barrier moves; it does not go away.** Some scope still waits for the
> result. `join_at_scope` chooses which one.

## When to use it

- **A slow worker whose answer is needed later, not next.** Research, a build,
  a model behind a worker — while the scopes before the consumer (checks,
  request shaping, logging, other lookups) do not depend on it.
- **Two slow workers at once.** Dispatch both with the same `join_at_scope`
  and both results are merged before that scope runs.

Use plain `mode = "async"` when the very next scope needs the answer. Use
`mode = "continuable"` when a quick answer should stay synchronous — the two
don't combine (see [Rules](#rules)).

## The shape

```txcl
# OPS/site/100/research.txcl — dispatch now, join at 200
WITH mode = "async", join_at_scope = 200, timeout = "20m"
EXEC "https://research.example.com/start"
```

```txcl
# OPS/site/150/audit.txcl — runs while the research is in flight
EMIT .audit.started = true
```

```txcl
# OPS/site/200/render.txcl — the research output is already merged
WHEN .research.done == true
EXEC "https://render.example.com/page"
```

The worker contract is the ordinary async one from
[Continuations](../continuations.md): the chassis POSTs the op's input with a
`callback_url`, an expiry and a single-use token; the worker acks `202`; later
it POSTs `{"status": "completed", "output": {…}}` — or `"failed"` with an
`error` — to the callback URL.

## What happens, step by step

1. **Dispatch.** At scope 100 the op is taken out of its scope and sent to the
   worker without waiting for the result. The other ops at scope 100 run as
   usual. The first deferred op on a request creates a durable run and freezes
   the stack's ops for it, so a resumed flow runs the ops as they were at
   dispatch even if you `txco apply` in between.
2. **The scopes in between run.** 110, 150, … run in the request as normal.
   They do **not** see the worker's result — nothing is merged yet.
3. **The join check, at every scope.** Before a scope's `WHEN` clauses are
   evaluated, the chassis looks at every outstanding join whose floor has been
   reached: the first scope **at or above** `join_at_scope` that the flow
   actually reaches (scope 200 does not have to exist; 250 would do).
   - **The result is already in** → it is merged into the document, and that
     scope's `WHEN` clauses see it. The request finishes synchronously; the
     client never sees a `202`.
   - **The worker is still busy** → the flow suspends at that scope, exactly
     as a plain async op would: a web client gets the `202` and poll URL (a
     browser gets the wait page), an LMTP session is closed with `250` unless
     a verdict was already emitted ([non-web inlets](../continuations.md#fast-when-it-can-be-continuable)).
   - **The op failed** (the worker reported failure, or never acked) → the
     request ends with that error; the join scope and everything after it do
     not run.
4. **Resume.** When the worker calls back, its output is merged and the join
   scope **runs its own ops** — they never ran before the suspend. The flow
   then continues from there. This is the difference from a plain async op,
   whose scope is already done when it resumes.

Each result is merged **at most once**, even when a callback races the
in-request check. The merge is the normal [operation](../ops.md) merge:
objects deep-merge, arrays append, and callback output is sanitized like any
async worker output. Several ops joining at one scope merge in a stable order
(sorted by op name), not in the order they finished.

## Rules

- **`join_at_scope` is an integer greater than the op's own scope.** A value
  at or below the op's scope is not an error: the op simply behaves as a plain
  async op and suspends at its own scope. `txco apply` does not check the
  value.
- **Only with `mode = "async"`,** which applies to `http(s)://` and
  `mcp+http(s)://` targets. On a `mode = "continuable"` op the op fails at
  dispatch with `join_at_scope (= N) requires mode = "async"; it is not
  honored with mode = "continuable"`. An async op can't carry a `LOOP`
  (`txco apply` rejects it).
- **One stack.** Scope numbers mean something only inside the stack that
  dispatched the op. If the flow moves to another stack (`@goto`, a stage jump)
  before reaching the floor, every outstanding join resolves at the first scope
  of the new stack — merged if ready, otherwise the flow suspends there.
- **Keep the scopes in between independent.** A scope between dispatch and
  join that reads a path the worker writes sees nothing, and nothing warns you.
- **Keep the join scope synchronous.** A join scope that also holds its own
  `mode = "async"` op is a known limitation: both suspends use the same scope
  key, and only the deferred one is kept. Put the consumer at the join scope
  and any further async work one scope later.
- **Other ops in the same request are fine.** Same-scope async ops and
  `mode = "continuable"` ops later in the flow reuse the deferred op's run
  rather than starting a second one.

## When things go wrong

| Situation | What happens |
|---|---|
| The worker never calls back | The flow waits at the join. The run expires at dispatch + the op's `timeout` + `--deferred-join-slack`; the continuation sweeper then fails it and a polling client sees the failure. |
| The worker doesn't ack within `--async-ack-timeout`, or the dispatch errors | Recorded as failed at once. The scopes in between still run; the request ends with the error when it reaches the join. |
| The worker reports `"failed"` | In the request: the request ends with the worker's error. After a suspend: the resume records a failed stage and the run fails. |
| `@halt` before the join, or the stack ends with no scope at or above `join_at_scope` | The response goes out **without** the result. The worker keeps running; its callback is recorded but never merged. Nothing warns. |
| A backward `@goto` loops below the floor | The join stays outstanding until the flow crosses the floor or the run expires. |
| The chassis restarts before the flow reaches the join | The client connection is gone and the run is not driven forward again; it expires and the result is discarded. Once the flow has **suspended** at the join, a restart is safe — the callback resumes it, on whichever node receives it when the continuation store is shared. |

There is no hard deadline ("fail if not ready when the join is reached"), no
cancellation of the worker when the flow gives up, and no check that the
scopes in between are independent.

## Timeouts

| Knob | Default | Meaning |
|---|---|---|
| `WITH timeout` | `--async-runtime-default` (10m) | How long the worker has. Sets the run's expiry (plus the slack below). Not capped by `--op-timeout-max` on an `http(s)://` op; an `mcp+` op runs inside the chassis and is capped. |
| `--async-ack-timeout` | 5s | How long the chassis waits for the worker's `202`. |
| `--deferred-join-slack` | 60s | A flat pad on the expiry for the scopes the flow still has to run after the join. |
| `--continuation-retention` | 7 days | How long a finished run's records are kept. |

## Costs

- **Scopes.** Every scope the flow walks pays its normal
  [fuel](./fuel.md). While a join is outstanding the chassis enters each scope
  one at a time (no skipping ahead over empty scopes) so the join check runs
  at every boundary — more work per request, the same fuel.
- **The dispatch.** An `http(s)://` async op — deferred or not — is not
  charged the per-EXEC fuel a synchronous call pays, and the worker's own time
  is not metered. An `mcp+` async op runs inside the chassis on its own budget
  (the EXEC charge included); that fuel is added to the flow when it joins.
- **The first deferred op on a request** writes a run record and a snapshot of
  the tenant's ops, the same cost as any continuation suspend.
- **A resumed flow** is billed as its own segment, like any continuation.

## Reading a trace

- **The dispatch scope** records the op as a `pending` step (transport
  `async`) with the worker's ack; a failed dispatch is an `error` step.
- **An in-request merge** has no step of its own — the merged fields simply
  appear in the join scope's input.
- **A suspend** adds a `continuation.suspend` event with the run id and stage.
- **A resume** is a separate trace whose id starts with `resume-<run id>`. It
  opens with a `continuation.resume` event (carrying the original request's
  id), then one step per joined op, then the join scope's own ops. See
  [Trace internals](./trace.md) and [Visibility](../visibility.md).
