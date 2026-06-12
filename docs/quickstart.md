# Quickstart

_Thanks, Computer (TxCo) runs parts of your work as small, readable
rules: events arrive from any protocol, matching operations fire in
parallel, and their JSON outputs merge into one answer.
([Overview](./overview.md))_

Install, see it run, learn the model — about 2 minutes. (Authoring and
deploying a stack of your own is closer to 10: [Running a chassis](./running.md).)

## 1. Install

```sh
brew tap loremlabs/txco && brew install txco
```

or

```sh
curl -fsSL https://get.thanks.computer/install.sh | bash
```

## 2. See it run

```sh
txco demo
```

Zero config: this boots a throwaway local **chassis** — the TxCo runtime,
one binary — and opens a demo in your browser. Author rules, fire events,
and inspect the full trace of each flow without leaving the page.

![The txco demo playground in the browser](https://github.com/user-attachments/assets/696fce36-17ae-4609-807d-723450a3c6bc)


## 3. The mental model

An event is a JSON document flowing through **steps**. At each step, every
matching **operation** runs in parallel; each returns JSON; the outputs merge
back into the event, which carries on to the next step.

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

Three things make this workable:

- **JSON in, JSON out.** An operation is anything that takes the event and
  returns JSON — `EXEC "https://…"` makes any HTTP service in any language
  an op.
- **Namespaces, not locks.** Parallel ops coordinate by writing to their own
  part of the document; the merge combines them. No shared mutable state to
  guard.
- **Resonators keep ops quiet.** Each operation is gated by a `WHEN`
  condition — most ops don't fire on most events. Only what matches runs.

A complete operation is a few lines:

```txcl
WHEN @web.req.url.path == "/hello"
EMIT .greeting = "Hello from the chassis!"
```

(`@` reads the envelope — the chassis's metadata around your JSON, like the
request path; plain `.greeting` writes the payload.)

## Next

- [Tutorial](./tutorial.md) — one real flow built end to end: mail in, AI
  draft, human approval, reply sent
- [Operations](./ops.md) — the three shapes of an op, and how to write one
  in your own language over HTTP
- [TXCL](./txcl.md) — the full rule language
- [Running a chassis](./running.md) — `txco serve` and the author–apply
  loop, when you're ready to run your own
- [Arcs](./arcs.md) & [Sagas](./sagas.md) — the matters your rules manage,
  and the missions those matters serve
- Complete, runnable workspaces live in [`examples/`](../examples/) —
  an inbound support mailbox, a Stripe enrichment flow, MCP, and more
