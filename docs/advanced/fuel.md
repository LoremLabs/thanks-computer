# Fuel and TTL — loop and cost budgets

_Operator reference for the per-request budget guards: how the chassis
stops runaway loops and expensive work, and how to read the errors when
it does._

Every request carries two budgets, working as a pair:

| Guard | Envelope field   | Question it answers      | Exhaustion error           |
| ----- | ---------------- | ------------------------ | -------------------------- |
| TTL   | `_txc.ttl`       | "Is this a loop?"        | `txcl_scope_ttl_exhausted` |
| Fuel  | `_txc.fuel_used` | "Is this expensive work?"| `txco_fuel_exhausted`      |

The two codes are deliberately distinct so an operator can tell a loop
from costly-but-legitimate work at a glance. Implementation:
`chassis/processor/budget.go`.

## TTL — the hop counter

`_txc.ttl` is a countdown, decremented once per stage entry (every
scope advance, `@goto`, or stage-jump `EXEC`). It starts at
`--op-scope-ttl-max` (default `500`); `0` disables the guard.

A rule may voluntarily *lower* its remaining budget —
`EMIT @ttl = 20` before entering a polling loop — but can never raise
it (the IP-TTL idiom: writes are clamped to the current value). Use
this to give a known-risky subflow a tight sub-budget without touching
the chassis-wide cap.

## Fuel — the work meter

`_txc.fuel_used` counts up against `--max-fuel-per-request` (default
`100000`; `0` = unlimited). Costs are weighted per action:

| Action                       | Fuel        |
| ---------------------------- | ----------- |
| Entering a scope             | 10          |
| `EXEC` dispatch              | 25          |
| Nano-op compute, per ms      | 10          |
| Secret materialization       | 100         |
| Repeated stage transition    | 50          |

Calibration: 1 fuel ≈ 100 µs of typical chassis work. So a 1 ms
nano-op costs 35 total (25 dispatch + 10 compute); an op wrapping a
1-second LLM call costs ~10,000 — meaning the default cap tolerates
roughly ten such calls per request before cutting off.

The final fuel value is logged on the per-request `usage` line
(`fuel=N`) — single-tenant deployments can ignore it; tenant-aware
deployments aggregate it for quota or billing.

## Repeat transitions: backpressure before the kill

The chassis keeps a per-request seen-set of stage transitions
(`"from->to"`, carried as `_txc._seen`). The first time a transition
happens it's free; every repeat charges 50 fuel **and** sleeps
`--op-repeat-penalty-ms` (default `20`, `0` disables) before
proceeding. A tight loop therefore degrades gracefully — it slows
down, burns fuel measurably, and shows up in traces — rather than
spinning the CPU until the hard cap lands.

## Envelope mechanics

All three fields (`_txc.fuel_used`, `_txc.ttl`, `_txc._seen`) ride the
envelope, so budgets propagate through `@goto`, `EXEC` stage jumps,
continuations, and deferred-join fan-out the same way `_txc.tenant`
does — a flow can't shed its budget by jumping stacks. They are
stripped from the response before it reaches the inlet client (after
the fuel value is captured for usage accounting).

## Reading an exhaustion

Both errors return a structured JSON payload as the request's final
response, including the stage where the request gave up and the last
three transitions — usually enough to spot the cycle without opening a
trace:

```json
{"code":"txco_fuel_exhausted","max_fuel":100000,"fuel_used":100025,
 "last_stage":"billing/0","last_transitions":["retry/0->billing/0","billing/0->retry/0","retry/0->billing/0"]}
```

```json
{"code":"txcl_scope_ttl_exhausted","max_ttl":500,"consumed":500,
 "last_stage":"poll/0","last_transitions":["poll/0->poll/0","poll/0->poll/0","poll/0->poll/0"]}
```

For the full step-by-step picture, pull the [trace](./trace.md) for
that rid.

## Apply-time lint: catching typos before runtime

The runtime guards always catch loops eventually; `txco apply` also
lints the assembled stack for the unambiguous mistakes
(`chassis/cli/loop_lint.go`):

- **Unconditional self-loop** — a rule with no `WHEN` and no
  `EMIT @halt` that `@goto`s or `EXEC`s back into its own
  `(stack, scope)`. Usually a typo: `@goto = "self/0"` meant as
  `"self/1"`.
- **Unconditional 2-stack ping-pong** — stage A unconditionally points
  to B, and B unconditionally back to A.

Warnings only (printed to stderr; `apply` proceeds). The lint is
deliberately conservative: conditional state-machine loops and
intentional polling pass unflagged — those are the runtime guards' job.

## Flags

| Flag                     | Default  | Meaning                                  |
| ------------------------ | -------- | ---------------------------------------- |
| `--max-fuel-per-request` | `100000` | Fuel cap; `0` = unlimited                |
| `--op-scope-ttl-max`     | `500`    | Starting hop budget; `0` = disabled      |
| `--op-repeat-penalty-ms` | `20`     | Sleep per repeated transition; `0` = off |
