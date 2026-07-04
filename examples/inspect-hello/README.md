# inspect-hello

The smallest possible **inspector** — a stack that explains its own state.
`txco trace` answers "what just happened?"; `txco inspect` answers "what is
the current state, and why?". An inspect request becomes a normal event
(`@src == "inspect"`), routed to this workspace's `_inspect` stack, which
answers with a structured **card** the CLI renders as aligned sections.

## Run it

Against a running chassis (e.g. `txco dev` in this directory):

```sh
txco apply

txco inspect demo user matt@example.com          # rendered card
txco inspect demo user matt@example.com --json   # raw card document
```

Or hit the inlet directly (dev chassis, open on loopback):

```sh
curl -s -XPOST http://localhost:8081/v1/tenants/default/inspect \
     -H 'content-type: application/json' \
     -d '{"stack":"demo","noun":"user","id":"matt@example.com"}'
```

## The card contract

An inspector op gates on the `stack`/`noun` it knows how to explain and EMITs
`._inspect.card`:

```json
{
  "title": "Demo Inspector",
  "sections": [
    { "title": "Request",
      "rows": [ ["Stack", "demo"], ["Noun", "user"], ["Id", "matt@example.com"] ] }
  ],
  "raw": { }
}
```

- `rows` are `[label, value]` pairs; a value may be any JSON.
- `raw` (optional) carries the underlying domain JSON for `--json` consumers.
- The request context rides read-only at `@inspect.{stack,noun,id,args}` —
  chassis-stamped, so an op cannot forge the tenant or the question.

Replace the echo with real lookups — `txco://kv/get` a per-user snapshot,
`txco://read-file` a manifest, shape the card in a nano-op. A package that
ships an `_inspect/` subtree makes every install debuggable: only the stack
knows its own keying, so the stack owns the explanation.

## What's inside

| File | What |
|------|------|
| `OPS/_inspect/100/card.txcl` | The inspector: WHEN `@src == "inspect"` → EMIT `._inspect.card` |
| `txco.yaml` | Dev target (`txco dev` chassis on `localhost:8081`) |
| `probe.json` | Smoke-harness note (admin-API inlet → dry-run only) |
