# Thanks, Computer — Documentation

_Thanks, Computer (TxCo) runs parts of your work as small, readable
rules: events arrive from any protocol, matching operations fire in
parallel, and their JSON outputs merge into one answer._

```
          event (JSON)
               │
  step 1  ┌────┼────┐
          ▼    ▼    ▼
        ┌───┐┌───┐┌───┐
        │op ││op ││op │     run in parallel
        └─┬─┘└─┬─┘└─┬─┘
         {a}  {b}  {c}      each returns JSON
          └────┼────┘
             merge          event now has a, b, c
               │
  step 2      ...
```

Read in order, or jump to what you need:

1. **[Overview](./overview.md)** — what Thanks, Computer is, and where
   it's going.
2. **[Quickstart](./quickstart.md)** — install, `txco demo`, and the
   model. About 2 minutes.
3. **[Arcs](./arcs.md)** — the unit of attention: the ongoing matters
   the platform manages.
4. **[Sagas](./sagas.md)** — the level above: missions that span arcs,
   carrying the *why* into the work.
5. **[Operations](./ops.md)** — the unit of work: three shapes, one
   JSON-merge contract, any language.
6. **[TXCL](./txcl.md)** — the rule language: when an operation fires
   and what it contributes.
7. **[Ingress](./ingress.md)** — every protocol, one flow: web, email,
   cron, TCP, and AI agents.
8. **[Continuations](./continuations.md)** — built for waiting: how an
   operation suspends a flow and calls back to resume it.
9. **[AI](./ai.md)** — `ai://chat`: a model as an operation, prompts
   that read the document, structured output.
10. **[Trace](./trace.md)** — see exactly what a flow did, after the
    fact.
11. **[Schemas](./schemas.md)** — optionally write down the shape a
    stack reads and writes, for humans and machines.
12. **[Packages](./packages.md)** — share a working department; install
    someone else's.
13. **[Running a chassis](./running.md)** — `txco serve` and the
    author–apply loop, on your own machine.

Building stacks day to day? The **[authoring guides](./authoring/README.md)**
cover the workspace layout, the `txco dev` loop, mocks, and nano-ops.

Operating in production? The **[advanced references](./advanced/README.md)**
hold the fact-level detail: the full flag surface, the admin API, the
[per-protocol pages](./advanced/protocols/README.md) (web, mail in/out,
cron, TCP, MCP, routing), secrets, trace internals, and the complete
TXCL reference.
