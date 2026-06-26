# Thanks, Computer — Documentation

_Thanks, Computer (TxCo) is an event-driven runtime for durable,
human-in-the-loop workflows: events arrive from any protocol, matching
operations fire in parallel, and their JSON outputs merge into one
answer — and a person, or a slow AI, can pause a flow and resume it
exactly once._

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
3. **[Tutorial: hello, world](./tutorial/hello-world.md)** — hands-on, end to
   end on the hosted service: sign in, pull a package, deploy, get a public URL.
4. **[Operations](./ops.md)** — the unit of work: three shapes, one
   JSON-merge contract, any language.
5. **[Resonators](./resonators.md)** — the trigger condition that gates each operation:
   when it fires and what it contributes.
6. **[Continuations](./continuations.md)** — built for waiting: how an
   operation suspends a flow and calls back to resume it.
7. **[AI](./ai.md)** — `ai://chat` and `ai://embed`: a model as an operation,
   prompts that read the document, structured output, embeddings.
8. **[Vectors](./vectors.md)** — the vector store: semantic search as
   operations, and deploying a catalog declaratively with `txco data`.
9. **[Visibility](./visibility.md)** — see exactly what a flow did, after the
   fact.
10. **[Schemas](./schemas.md)** — optionally write down the shape a
    stack reads and writes, for humans and machines.
11. **[Packages](./packages.md)** — share a working department; install
    someone else's.
12. **[Domains](./advanced/protocols/dns.md)** — delegate a subdomain and the chassis
    runs its DNS: mail records, reputation keys, TLS, handled.
13. **[Tenants](./tenants.md)** — one chassis, many isolated worlds:
    stacks, domains, secrets, people, and usage, walled per tenant.
14. **[Running a chassis](./running.md)** — `txco serve` and the
    author–apply loop, on your own machine.

Building stacks day to day? The **[authoring guides](./authoring/README.md)**
cover the workspace layout, the `txco dev` loop, mocks, and nano-ops.

