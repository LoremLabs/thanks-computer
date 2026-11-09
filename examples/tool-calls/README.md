# tool-calls — a model calls the tenant's capabilities, natively

`GET /ask?sku=abc` offers a model two of this tenant's capabilities as
**tools**, lets it ask for one, runs the call through `cap://`, hands the
result back, and answers with what the model said. Nothing in the stack
describes a tool in prose or parses a made-up JSON action: the model's own
tool-calling takes the definitions and returns the calls.

```
OPS/front/
  100/tools.txcl         txco://caps/list WITH names = [...], format = "tools"   → _tools
  200/ask.txcl           ai://chat WITH prompt, tools = ._tools.tools, into = "_turn"
  210/order.txcl         the same, for a question that needs two tools
  220/bad.txcl           the same; the mocked model sends arguments the schema refuses
  300/check0_*.txcl      txco://schema/check: the call's arguments against its tool's parameters
  300/check1_quote.txcl  the second call's, beside the first
  310/run0.txcl          cap:// WITH name = ._turn.tool_calls.0.binding.capability   → _res0
  320/run1.txcl          the second call, after the first                            → _res1
  400/final_one.txcl     ai://chat WITH messages = [user, ._turn.message, tool result]
  410/final_two.txcl     the same with both results
  900/answer.txcl        200: the answer, the calls, the capabilities' answers
  900/refused.txcl       422: the arguments did not match (the problems, with paths)
  900/chat_error.txcl    502: the model call failed
OPS/_tools/              `_`-named: only capability calls enter
  CAPS/stock.check.yaml  typed input: sku (string, required); the tool `stock_check`
  CAPS/price.quote.yaml  typed input: sku, qty (integer 1–100, default 1); the tool `price_quote`
  300/, 310/, 400/, 410/ the capabilities' rules
```

The round trip uses public ops only:

```
caps/list ──► ai://chat ──► schema/check ──► cap:// ──► ai://chat ──► answer
```

- **The catalogue is the tool registry.** `caps/list WITH format = "tools"`
  derives each definition from the declaration: the name with dots as
  underscores, the typed input as a JSON Schema, and a `binding` naming the
  capability. Adding a tool is declaring a capability.
- **`ai://chat` does model I/O only.** It sends the definitions in the
  provider's form and returns the calls; it copies each call's binding from
  the definition *this request* offered (a provider cannot smuggle one in),
  flags a call to a tool that was not offered, and refuses a transcript
  with a call left unanswered. It never runs a call.
- **The executor validates and runs.** `schema/check` checks the arguments
  against the tool's parameters; `cap://` runs the capability its binding
  names, under the capability's own rules (and, from a node, the grant
  gateway). A failed check never reaches the capability (`/order-bad`).
- **Several calls in one turn** are run in order and all answered before
  the next request (`/order`).

## Run it

Hermetic, with the model's answers served from the scopes' fixtures: start
dev with the mock header allowed (chassis flags are environment variables
under `txco dev`) and ask for the fixtures per request.

```
TXCO_WEB_MOCK_HEADER=true txco dev          # from this directory
curl -H 'X-Txco-Mocks: front/200/*,front/400/*' 'http://localhost:8080/ask?sku=abc'
curl -H 'X-Txco-Mocks: front/210/*,front/410/*' 'http://localhost:8080/order?sku=abc&qty=3'
curl -H 'X-Txco-Mocks: front/220/*' 'http://localhost:8080/order-bad?sku=abc'
curl 'http://localhost:8080/tools'
```

```json
{"answer":"Yes: SKU abc is in stock.",
 "calls":[{"id":"call_stock_1","name":"stock_check","arguments":{"sku":"abc"},"binding":{"capability":"stock.check"}}],
 "stock":{"in_stock":true,"sku":"abc"},
 "cap":{"ms":0,"name":"stock.check","status":200}}
```

Live, against OpenAI: set the key (dev reads a backend's key from the
environment when the tenant has none stored) and drop the header. Nothing
in the stack changes.

```
OPENAI_KEY=sk-... txco dev
curl 'http://localhost:8080/ask?sku=abc'
```

`make smoke` runs `probe.json`: the hermetic checks above, through the same
stack, with `EXAMPLES_ONLY=tool-calls` to run this one alone.
