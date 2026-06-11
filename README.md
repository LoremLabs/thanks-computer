# Thanks, Computer (TxCo)

_Thanks, Computer_ is a distributed runtime for coordinating business logic across systems, services, humans, and time.

New here? The **[guided docs](docs/README.md)** walk from overview to a
working stack — including a [2-minute quickstart](docs/quickstart.md) and an
[end-to-end tutorial](docs/tutorial.md).

Where an operating system coordinates processes on a single machine, TxCo coordinates operations across an event flow:

```
            ●  event in
            │
         ┌──┴──┐
         │flow │
         └──┬──┘
      ┌─────┼─────┐
      ▼     ▼     ▼
    ┌───┐ ┌───┐ ┌───┐
    │op │ │op │ │op │   parallel execution
    └─┬─┘ └─┬─┘ └─┬─┘
      └─────┼─────┘
         merge
           │
           ▼
          ● output
```

An incoming event (HTTP, email, cron, queue, webhook, etc.) enters an
operation stack (OpStack).

At each step, TxCo evaluates resonators — small rules written in [txcl](docs/txcl.md) that determine
which operations should run. Matching operations execute in parallel,
their outputs deep-merge into a shared event document, and the flow
continues to the next step.

## The model

TxCo separates three concerns:

- Flow — when and under what conditions operations run
- Execution — the actual code or external service being invoked
- Merge — combining results back into the event flow

This separation makes it possible to coordinate workflows that span:

- multiple services
- multiple organizations
- humans and AI systems
- asynchronous and long-running processes

Operations stay small and composable, while the OpStack defines how they
cooperate as a larger system — similar to how departments coordinate work
inside an organization.

## Inspiration

TxCo draws inspiration from early distributed AI systems and event-driven
architectures, particularly:

- [Blackboard Architecture](https://en.wikipedia.org/wiki/Blackboard_system)
- Oliver Selfridge's "Pandemonium" model of independent cooperating agents

These systems explored how complex behavior can emerge from many small,
specialized processes operating over shared state.

## License

[Mozilla Public License 2.0](./LICENSE).
