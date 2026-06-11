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
3. **[Tutorial](./tutorial.md)** — one real flow built end to end:
   mail in, AI draft, human approval, reply sent.
4. **[Arcs](./arcs.md)** — the unit of attention: the ongoing matters
   the platform manages.
5. **[Sagas](./sagas.md)** — the level above: missions that span arcs,
   carrying the *why* into the work.
6. **[Operations](./ops.md)** — the unit of work: three shapes, one
   JSON-merge contract, any language.
7. **[TXCL](./txcl.md)** — the rule language: when an operation fires
   and what it contributes.
8. **[Ingress](./ingress.md)** — every protocol, one flow: web, email,
   cron, TCP, and AI agents.
9. **[Continuations](./continuations.md)** — built for waiting: how an
   operation suspends a flow and calls back to resume it.
10. **[AI](./ai.md)** — `ai://chat`: a model as an operation, prompts
    that read the document, structured output.
11. **[Trace](./trace.md)** — see exactly what a flow did, after the
    fact.
12. **[Schemas](./schemas.md)** — optionally write down the shape a
    stack reads and writes, for humans and machines.
13. **[Packages](./packages.md)** — share a working department; install
    someone else's.
14. **[Domains](./domains.md)** — delegate a subdomain and the chassis
    runs its DNS: mail records, reputation keys, TLS, handled.
15. **[Tenants](./tenants.md)** — one chassis, many isolated worlds:
    stacks, domains, secrets, people, and usage, walled per tenant.
16. **[Running a chassis](./running.md)** — `txco serve` and the
    author–apply loop, on your own machine.

Building stacks day to day? The **[authoring guides](./authoring/README.md)**
cover the workspace layout, the `txco dev` loop, mocks, and nano-ops.

Operating in production? The **[advanced references](./advanced/README.md)**
hold the fact-level detail: the full flag surface, the admin API, the
[per-protocol pages](./advanced/protocols/README.md) (web, mail in/out,
cron, TCP, MCP, routing), secrets, trace internals, and the complete
TXCL reference.
