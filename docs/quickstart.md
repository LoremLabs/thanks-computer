# Quickstart

_Thanks, Computer (TxCo) runs parts of your business as small, readable
rules: events arrive from any protocol, matching operations fire in
parallel, and their JSON outputs merge into one answer.
([Overview](./overview.md))_

Install, see it run, learn the model. About 2 minutes.

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

Zero config: this boots a throwaway local chassis and opens a demo in your
browser — author rules, fire events, and inspect the full trace of each flow
without leaving the page.

## 3. The model

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

## Next

- [Operations](./ops.md) — the three shapes of an op, and how to write one
  in your own language over HTTP
- [TXCL](./txcl.md) — the full rule language
- [Running a chassis](./running.md) — `txco serve` and the author–apply
  loop, when you're ready to run your own
