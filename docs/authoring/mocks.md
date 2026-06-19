# Mocks — build the flow before the services exist

_Every operation can carry a canned response. That lets you author and
run an entire stack — routing, merging, tracing — before any backend
is written, and lets a caller swap real ops for fixtures per request._

## The fixtures

Drop two files next to a rule; `txco apply` attaches them to every
rule in that scope:

```yaml
OPS/support/0100_TRIAGE/
  classify.txcl
  mock-request.json     # the input this op expects — documentation + test input
  mock-response.json    # the output to serve when mocked
```

`mock-request.json` is never consulted at runtime — it documents the
expected input and feeds `txco op test`. `mock-response.json` is the
live fixture.

## Two ways a mock fires

**Author-pinned** — point the rule at the mock explicitly while the
real target doesn't exist yet:

```txcl
WHEN .kind == "invoice"
EXEC "txco://mock"        # serves this scope's mock-response.json, verbatim
```

Swap to the real `EXEC "https://…"` later; nothing else changes.
(`txco://mock` with no fixture fails loudly — it's a typo, not a
fallback.)

**Caller-driven** — the request itself asks for mocks, per op, by glob
pattern on `<stack>/<scope>/<name>`:

```sh
curl -H 'X-Txco-Mocks: support/**,!support/0200/notify' http://localhost:8080/…
```

Patterns ride the envelope as `_txc.mocks` (the header form needs
`--web-mock-header` on). Matching ops serve their fixture instead of
dispatching; `!` excludes (last match wins); a matching op with no
fixture falls through to its real EXEC. This is integration testing
against the real chassis with chosen edges stubbed.

## Keeping mocks out of production

In `txco.yaml`, a target can declare:

```yaml
targets:
  prod:
    chassis: https://chassis.example.com:8081
    mock: deny
```

`mock: deny` makes `apply`/`push`/`dev` strip every `mock_res` before
upload — production literally cannot serve a fixture, even if asked.
The default is `allow`.

## Mocks in `txco op test`

For nano-ops, the same fixtures drive local tests:
`txco op test` runs the compute with `mock-request.json` as input and,
if `mock-response.json` exists, diffs the output against it (exit 1 on
mismatch — CI-friendly). `mock-env.json` and `mock-secrets.json`
supply `ctx.env` / `ctx.secrets` locally. See
[nano-ops.md](./nano-ops.md).
