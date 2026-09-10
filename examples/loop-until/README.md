# loop-until — drain a cursor inside one op

`GET /drain` seeds five keys into a KV namespace, then **one op** lists
them two at a time until the cursor runs out — three passes inside a
single dispatch, one merged payload — and answers with every key plus
the loop's bookkeeping.

```
OPS/kv-drain/
  100/seed_*.txcl     five parallel txco://kv/set ops (idempotent)
  200/drain.txcl      txco://kv/list … LOOP EVERY "2ms" UNTIL ._page.next == "" MAX 10
  300/respond.txcl    copies the keys + @runtime.loop.drain onto the body
```

Run it:

```
txco dev          # from this directory
curl http://localhost:8080/drain
```

```json
{"keys":["key-a","key-b","key-c","key-d","key-e"],"loop":{"passes":3,"stop":"done","elapsed_ms":5}}
```

What the loop does, pass by pass: the op's WITH values are re-resolved
against the *view* (the envelope plus everything the loop has merged so
far), so `after = ._page.next` advances; the pass output merges with the
usual scope-merge rules (arrays append, scalars overwrite); then the
predicate is checked; then, only if the loop continues, it pauses
`EVERY` and goes again. It stops for one of six reasons — `done`, `max`,
`timeout`, `fuel`, `halted`, `error` — none of which fails the run; the
reason lands at `_txc.runtime.loop.<op name>` for a later scope to gate
on. The trace shows one step for the whole loop with `passes` and
`stop_reason`:

```
txco trace <rid> --step drain
```

The simplest LOOP is a poll: `EXEC "https://…/jobs/42" LOOP UNTIL
.status == "done"` — ten checks, 50ms apart, inside a minute. For a
cursor that must travel in a POST body, `LOOP SET .cursor = ._items.next
UNTIL …` writes it onto the input from the second pass on.

See [txcl — LOOP](../../docs/advanced/txcl/txcl.md#loop--repeat-an-op).
