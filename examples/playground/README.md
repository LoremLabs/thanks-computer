# playground

Four example stacks in **one** workspace, routed by URL path. Start it
once and poke all of them — no per-stack hostname binding, no re-apply,
no switching directories.

| Path | Stack | What it is |
|------|-------|------------|
| `/` (and anything else) | `hello-world` | a multi-stage pipeline over two local Node apps |
| `/mcp` | `mcp-server` | a JSON-RPC MCP server written entirely in txcl (no app) |
| `/stripe/*` | `stripe` | verify a signed Stripe webhook, then call the API back to enrich it |
| `/stream` | `stream-demo` | an HTTP response **streamed** chunk-by-chunk as backend steps finish |

Each of these also ships standalone (`quickstart-hello-world`,
`mcp-server`, `stripe-customer-enrich`) if you want the minimal copy of
just one. The playground composes them to show **path-based routing as a
boot-pipeline hook**.

## How the routing works

The interesting part is `OPS/_sys/boot/25/route-*.txcl`. The `_sys/boot`
pipeline is split decide→execute, and **scopes 1–99 run for every request
and may rewrite the route proposal** before scope 100 executes it. These
rules sit at scope 25 and stamp `_txc.route.*` by path:

```
request → detect-tenant@0   no hostname binding in dev → empty proposal
        → route-by-path@25  EMIT @route.stack/to by URL path     ← the router
        → static@50         serves a routed stack's FILES/ (reads @route.stack)
        → route@100         WHEN @route.to != "" → re-tenant + goto
        → 1000              unrouted 404 (never reached here)
```

Two design points worth seeing in the files:

- **Scope 25, before static@50** — not concurrent with it. `txco://static`
  *reads* `_txc.route.stack` to serve a routed stack's own `OPS/<stack>/FILES/`,
  so it's a consumer of the route decision; the router has to run first.
- **`EMIT`, not `SET`** — a boot hook has no `EXEC`, and `SET` on a no-EXEC
  rule only shapes that op's own input and is dropped before the merge.
  `EMIT` persists across scopes.
- **Guarded `WHEN @route.to == ""`** — a real hostname binding (set via
  `txco auth tenant hostnames add`) wins; these are the fallback, so the
  pattern composes with production. Delete the `route-*@25` files and add
  bindings when you outgrow the single-workspace sandbox.

## Run it

Prerequisites: Node 18+ (`node:http`, no `npm install`), plus `curl` and
`openssl` for the Stripe sender.

```sh
cp -r examples/playground ~/playground
cd ~/playground
txco dev        # starts the chassis + api/worker/stripe-mock, applies all three stacks
```

Then in a second terminal:

```sh
# hello-world — just hit it
curl -s http://localhost:8080/ | head

# mcp-server — a JSON-RPC initialize
curl -s -X POST http://localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

# stream-demo — watch the body arrive in pieces (-N disables curl's
# buffering; -v shows `Transfer-Encoding: chunked`). "starting..."
# prints immediately, then a line after each ~700ms / ~800ms backend
# step, then "done." — instead of all at once at the end.
curl -N -v http://localhost:8080/stream

# stripe — needs its two secrets first (the CLI prompts on the TTY;
# use these demo values). The webhook sender signs with the same secret.
txco auth tenant secrets set STRIPE_WEBHOOK_SECRET     # paste: whsec_demo_secret
txco auth tenant secrets set STRIPE_API_KEY            # paste: sk_test_demo_key
export STRIPE_WEBHOOK_SECRET=whsec_demo_secret
./APPS/send-webhook.sh                                 # → 200, enriched customer
STRIPE_WEBHOOK_SECRET=wrong ./APPS/send-webhook.sh     # → 401 invalid signature
```

`txco trace last` shows any request's pipeline — including the scope-25
route decision, and (for the stripe path) that neither secret's bytes
appear anywhere.

## Notes

- No hostname binding needed — the path router is the sole router here. (A
  binding would take precedence; see the `WHEN @route.to == ""` guard.)
- All three stacks live in the dev tenant `default`; the router stamps
  that into the proposal.
- The stripe stack keeps the same simplifications as its standalone
  version (no replay/tolerance window; single `v1`, fixed order) — see
  `examples/stripe-customer-enrich/README.md`.
