# Aborting a run

A run is one request through a stack: it starts when an inlet hands the
chassis an envelope and ends when the last scope answers. Some runs are long
— an exec that builds something, a `@goto` loop that works through a list, a
`LOOP` that polls, a dispatch held open while another machine does the work —
and sometimes a person wants one to stop now, without stopping the chassis
and every other run on it. That is an abort.

```
txco runs                       # what is in flight, by rid
txco abort <rid>                # end one
txco abort --stack worker       # end every run of a stack
```

## What an abort is

A hard stop. The run's context is cancelled, with who asked as the cause:

- every op in flight is cancelled — a local exec is killed with its process
  group, an exec on a remote workspace is sent KILL, an HTTP or model call
  is abandoned, a `LOOP` stops between passes, a `@goto` loop does not
  re-enter;
- nothing later in the run executes: no next scope, no cleanup;
- the run's trace records each op that was running as `aborted`, and the
  request as `aborted` with the reason — `aborted by matt: closed the tab
  while running loop/500 workspace://pony/exec` — distinct from `cancelled`
  (a client that went away) and `timeout` (a deadline);
- a client still waiting is answered: over HTTP a `503` with
  `{"err":"aborted","error":{"code":"txco_run_aborted","message":…}}`.

There is no cleanup hook. A stack that must tidy up before it stops does so
first, then asks — that is what the builtin is for (below).

## Naming a run

Every run has a rid: the request id its inlet gave it, the one the trace
and the usage line carry and `@rid` reads in a rule. An HTTP inlet takes
it from an `x-request-id` header when the client sends one, so a client
that may want to abort what it started can choose the name in advance.

`txco runs` lists the tenant's runs in flight on the chassis — rid, inlet,
the stack the run was routed into, the scope it is in now, how long it has
been running — and `txco abort <rid>` ends one. `txco abort --stack <name>`
ends every live run that entered at the stack or is in one of its scopes
now; to keep new runs from starting as well, deactivate the stack first.
Both take `--reason`, which lands in the trace and the chassis log.

A run that is not in flight — it finished, or never existed — is
`run_not_live`. Aborting a run twice is a no-op.

Work that outlives its request is still the run: a `mode = "continuable"`
op that promoted at `continue_after` and runs on detached, after its client
got the 202, is listed and aborted under the same rid until it ends. A run
parked on a continuation (suspended, waiting for a callback) has no
goroutine to cancel and is not reached by this; nor is a WebSocket session.

## From a rule

```txcl
EXEC "txco://run/abort"
  WITH rid = ._run.dispatch_rid, reason = "stopped by the owner", into = "_abort"
```

`_abort = {aborted: 1, rid}`, or `_abort.error.{code, message}`. `WITH stack
= "<name>"` ends every live run of the stack instead. The calling tenant's
runs only; a run may abort itself. This is how an application builds its
own stop with its own bookkeeping first: record that the run was stopped
on purpose — so nothing reads it as abandoned and retries it — and only then
cut it.

The builtin reaches the process it runs in. On a fleet, use the admin
plane's abort for a run that may be on another node.

## On a fleet

Each node keeps its own registry of the runs it holds; the admin plane
does not know which node has a run. `txco abort` against the admin plane
ends the run if it is in that process and, on a fleet, publishes a
`run.abort` control event that every node applies to its own registry — on
all but one it ends nothing, which is the expected outcome. The answer is
`202` with `published: true` when the run was not in the admin plane's own
process; the node that holds it ends it a pump tick later. `txco runs` lists
the admin plane's own process; a fleet-wide list is not yet a thing.

## Capabilities and API

`run:*:read` lists, `run:*:abort` aborts; a tenant owner has both
(`run:*:*`). `GET /v1/tenants/{t}/runs`, `POST /v1/tenants/{t}/runs/{rid}/abort`,
`POST /v1/tenants/{t}/stacks/{name}/abort` with `{"reason": "…"}` — see the
[admin API](./advanced/admin-api.md#endpoint-map).
