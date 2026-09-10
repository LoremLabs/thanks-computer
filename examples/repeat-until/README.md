# repeat-until — drain a cursor inside one op

`GET /drain` seeds five keys into a KV namespace, then **one op** lists
them two at a time until the cursor runs out — three passes inside a
single dispatch, one merged payload — and answers with every key plus
the loop's bookkeeping.

```
OPS/kv-drain/
  100/seed_*.txcl     five parallel txco://kv/set ops (idempotent)
  200/drain.txcl      txco://kv/list WITH repeat_until = ._page.next == "", repeat_max = 10
  300/respond.txcl    copies the keys + @runtime.repeat.drain onto the body
```

Run it:

```
txco dev          # from this directory
curl http://localhost:8080/drain
```

```json
{"keys":["key-a","key-b","key-c","key-d","key-e"],"loop":{"passes":3,"stop":"done","elapsed_ms":1}}
```

What the loop does, pass by pass: WITH is re-resolved against the
*view* (the envelope plus everything the loop has merged so far), so
`after = ._page.next` advances; the pass output merges with the usual
scope-merge rules (arrays append, scalars overwrite); then the predicate
is checked. It stops for one of five reasons — `done`, `max`, `fuel`,
`timeout`, `error` — none of which fails the run; the reason lands at
`_txc.runtime.repeat.<op name>` for a later scope to gate on. The trace
shows one step for the whole loop with `passes` and `stop_reason`:

```
txco trace <rid> --step drain
```

See [txcl — repeating an op](../../docs/advanced/txcl/txcl.md#repeating-an-op--repeat_until).
