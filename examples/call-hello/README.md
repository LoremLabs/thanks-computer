# call-hello — one stack calls another and gets an answer

`GET /order` runs in `front`, which **calls** two things in one scope and
answers with both:

- `_pricing`, at its declared entry `quote`, with `call://` — a stack of
  this tenant, run in process on a fresh envelope, answering one result
  object;
- the capability `stock.check`, with `cap://` — declared by `_pricing` on
  this chassis, so it is answered here, on the same engine, in cap://'s
  own answer shape.

```
OPS/front/
  100/quote.txcl        EXEC "call://_pricing" WITH entry = "quote", input = …, into = "_quote"
  100/stock.txcl        EXEC "cap://stock.check" WITH input = …, into = "_stock"
  200/answer.txcl       ._quote.ok == true → 200 with the quote, who quoted, and the stock
  200/refused.txcl      ._quote.ok == false → 422 with the error (the callee's, or the engine's)
OPS/_pricing/           `_`-named: no inlet reaches it; only calls enter
  CALLS/quote.yaml      the entry: enters at 100, needs `amount`, promises `total`
  CAPS/stock.check.yaml the capability: enters at 300, needs `sku`, promises `in_stock`
  100/base.txcl         WHEN @src == "call" && @call.entry == "quote": total = amount
  150/discount.txcl     a FRIEND code takes 10 off
  160/refuse.txcl       an EXPIRED code: ._call.error, @halt (ends the callee, not the request)
  300/stock.txcl        WHEN @src == "cap" && @cap.name == "stock.check": in_stock
  310/out.txcl          SKUs starting with x are out of stock
```

Run it:

```
txco dev          # from this directory
curl 'http://localhost:8080/order?amount=40&code=FRIEND&sku=abc'
```

```json
{"quote":{"discount":10,"total":30},"quoted_by":"_pricing","quoted_by_version":1,
 "stock":{"in_stock":true,"sku":"abc"},"cap":{"ms":0,"name":"stock.check","status":200}}
```

(On localhost the dev chassis also shows the `_`-prefixed working keys,
`_quote` and `_stock` among them; a bound hostname renders only the plain
ones.)

```
curl 'http://localhost:8080/order?amount=40&code=EXPIRED&sku=abc'   # 422 {"error":{"code":"code_expired",…}}
curl 'http://localhost:8080/order?code=FRIEND&sku=abc'              # 422 {"error":{"code":"txco_call_invalid_input",…}}
curl 'http://localhost:8080/order?amount=40&sku=x9'                 # 200, "in_stock":false
txco calls list                                                     # the contract _pricing declares
txco caps list
```

What to notice:

- **The callee sees only its input.** `_pricing` starts from a fresh
  envelope: `@src == "call"`, `@call.input`, `@call.entry`, `@call.caller`,
  `@call.depth`. Nothing of `front`'s request reaches it, and nothing of
  its working fields comes back; the caller gets `{ok, output, error, stack,
  version, ms}` under `into` and nothing else.
- **A declared entry is a contract the chassis checks.** `CALLS/quote.yaml`
  says where the call enters and what it must carry and answer. Omit
  `amount` and the call is refused before `_pricing` runs
  (`txco_call_invalid_input`); had `_pricing` not written `total`, the
  answer would be `txco_call_invalid_output` with whatever it did write.
  `txco apply` refuses an entry that names no scope, and warns when a rule
  names an entry its callee lacks.
- **A refusal is data, not a failure.** `_pricing` writes `._call.error` and
  halts; the halt ends the callee only, and `front/200` goes on to answer
  422. Every engine verdict (`txco_call_timeout`, `txco_call_boundary`, …)
  arrives the same way.
- **One implementation, two ways in.** `stock.check` is declared under
  `CAPS/`, so a node dispatched by this chassis could call it over HTTP with
  a run grant; `front` calls it here, in process, and the same scope answers
  both. The in-process answer carries no `cap.rid`: it is this request.
- **The run is one request.** Both callees share `front`'s fuel, deadline
  and trace; `txco trace <rid>` shows their stages between `call.enter` and
  `call.return` events, each with the callee's version.

See [calls](../../docs/advanced/calls.md) and
[capabilities](../../docs/advanced/capabilities.md#answered-here).
