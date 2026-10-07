# allowance-hello — a tenant's own fuel budgets via `txco://allowance/*`

A tenant has its own fuel budget. An **allowance** is a budget the tenant
carves out of it for part of its work — one per customer, agent or feature —
as so much fuel per UTC hour, day or month. A stack puts a request in an
allowance, and from then on the request's fuel counts against it as well as
the tenant. When the window is spent, the next request is refused with
`429 allowance_exhausted` and a `Retry-After` until the window resets — the
same refusal a tenant over its rate limit gets — while the rest of the
tenant keeps running.

```
OPS/allowance-demo/
  100/set.txcl     POST /allowance/set?name=…&fuel=N[&per=…]  allowance/set
  100/get.txcl     GET  /allowance?name=…                      allowance/get
  100/enter.txcl   GET  /work?as=…   step 1: allowance/enter (its own scope)
  110/work.txcl                      step 2: the work the allowance pays for
  200/ok.txcl      answer with the op's object
```

Run it:

```
txco dev          # from this directory
curl -X POST 'http://localhost:8080/allowance/set?name=roomy&fuel=100000&per=hour'
curl -i 'http://localhost:8080/work?as=roomy'        # 200, "work":"done"
curl -X POST 'http://localhost:8080/allowance/set?name=tiny&fuel=1&per=hour'
curl -i 'http://localhost:8080/work?as=tiny'         # 429, Retry-After, x-txc-deny-reason: allowance_exhausted
curl -i 'http://localhost:8080/work?as=unlisted'     # 200: no definition = metered, never refused
curl 'http://localhost:8080/allowance?name=roomy'    # fuel, used, remaining, resets_at
txco allowance list                                   # the same, from the CLI
```

`tiny` is refused on its first request: entering an allowance lowers the
request's fuel ceiling to what the window has left (one fuel), and the very
next scope entry costs more than that. Usage reaches an allowance's counter
within about a second of a request finishing, so `used` lags the request
that just ran.

See [docs/advanced/allowances.md](../../docs/advanced/allowances.md).
