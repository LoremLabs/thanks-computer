# stripe-customer-enrich

A practical secret-store example: **receive a signed Stripe webhook,
verify it, then call the Stripe API back to enrich it** — using two
different secrets in the two ways the chassis consumes them.

It runs entirely offline. Two tiny local apps stand in for Stripe:
`send-webhook.sh` plays "Stripe calling you," and `stripe-mock` plays
`api.stripe.com`.

## What it demonstrates

| Step | Secret | How it's used |
|------|--------|---------------|
| Verify the inbound `Stripe-Signature` | `STRIPE_WEBHOOK_SECRET` | **computed** — `txco://hmac-verify` recomputes the HMAC and constant-time-compares it |
| Fetch the customer from Stripe | `STRIPE_API_KEY` | **substituted** — injected into the outbound `Authorization: Bearer …` header |

Neither secret ever appears in a rule, a log, or a trace — rules
reference secrets by **name**; the cleartext is materialized only at
dispatch and the trace shows a masked Bearer token and a boolean
verify result, never key bytes. That's the payoff: open the trace
after a request and confirm the secrets aren't in it.

## How it flows

```
send-webhook.sh ──POST /webhooks/stripe──▶ chassis (stack: stripe)
  Stripe-Signature: t=…,v1=…                100  verify.txcl
                                                 rebuild "t.body" from the raw body +
                                                 hmac-verify vs STRIPE_WEBHOOK_SECRET
                                                 → _txc.computed.sig_valid
                                            200  reject.txcl
                                                 WHEN not valid → 401 + halt
                                            300  fetch-customer.txcl
                                                 GET customer with STRIPE_API_KEY (Bearer)
                                                 ──▶ stripe-mock ──▶ {customer:{…}}
                                            (ends) → 200, enriched envelope as JSON
```

## Run it

Prerequisites: Node 18+ (`node:http`, no `npm install`), plus `curl`
and `openssl` for the sender.

```sh
cp -r examples/stripe-customer-enrich ~/my-stripe-demo
cd ~/my-stripe-demo
txco dev          # starts the chassis + stripe-mock, scaffolds OPS/_sys, auto-mints the secret master key
```

In a second terminal, from the same workspace:

```sh
# 1. Store the two secrets (the CLI prompts for the value on the TTY —
#    never on the command line). Use these exact demo values:
txco auth tenant secrets set STRIPE_WEBHOOK_SECRET     # paste: whsec_demo_secret
txco auth tenant secrets set STRIPE_API_KEY            # paste: sk_test_demo_key

# 2. Route a hostname to the stripe stack (a fresh chassis has none).
txco auth tenant hostnames add localhost --stack stripe

# 3. Fire a signed webhook. The sender signs with the SAME secret, so
#    verification passes. (export must match what you stored in step 1.)
export STRIPE_WEBHOOK_SECRET=whsec_demo_secret
./APPS/send-webhook.sh
```

Expected: **HTTP 200** with the enriched envelope, e.g.
`{"customer":{"id":"cus_demo123","email":"ada@example.com","name":"Ada Lovelace"},"customer_id":"cus_demo123"}`.

Then prove the negative and inspect the trace:

```sh
STRIPE_WEBHOOK_SECRET=wrong-secret ./APPS/send-webhook.sh   # → HTTP 401 invalid stripe signature
txco trace last                                             # see the steps — no secret bytes anywhere
```

## Simplifications (vs. real Stripe)

This example keeps the spotlight on the secret store, so it trims two
things a production verifier would add:

- **No replay/tolerance window.** Real Stripe rejects a webhook whose
  `t` is more than ~5 minutes old. That check is `now − t < 300`, which
  needs arithmetic txcl doesn't have yet — so it's omitted. Signature
  *integrity* is fully verified; replay-hardening is the follow-up.
- **Single `v1`, fixed order.** The header parse assumes `t=…,v1=…` in
  that order with one `v1`. Real Stripe can send several `v1` values
  during key rotation; a production rule would scan them.

Everything else — the `t.<raw body>` construction, HMAC-SHA256, and the
constant-time compare — is the real scheme.

## Adapting for production

- Point `STRIPE_GET_CUSTOMER` at `https://api.stripe.com/v1/customers`
  (see the commented `prod` target in `txco.yaml`).
- Store your real `whsec_…` and `sk_live_…`/`sk_test_…` values with the
  same `txco auth tenant secrets set` commands.
- Add a path guard (`WHEN @web.req.url.path == "/webhooks/stripe"`) if
  the stack's hostname also serves other traffic.
