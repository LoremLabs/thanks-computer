# inbound-support-mailbox

A worked example that turns inbound mail to `support@your.tenant` into classified ticket rows.

```
swaks → chassis LMTP → inbound-support/0   (accept per-recipient)
                     → inbound-support/100 (classify subject)
                     → inbound-support/200 (POST to ticketing service)
                                ↓
                            APPS/tickets logs the row
```

Postfix isn't needed to exercise the example — `swaks` speaks LMTP directly to the chassis socket. Wire Postfix in front when you want real mail flowing in; see [`docs/lmtp.md`](../../docs/lmtp.md#postfix-recipe).

## Run it

Prerequisites: Node 18+ (uses `node:http` — no `npm install`). `swaks` available (`brew install swaks` / `apt install swaks`).

```sh
cp -r examples/inbound-support-mailbox ~/my-mail-workspace
cd ~/my-mail-workspace

# LMTP is opt-in: add it to --personalities. The listener binds the
# default :2424 — no extra flag needed.
txco dev \
    --personalities=cron,tcp,web,admin,lmtp \
    --ingress-config=./ingress.yaml
```

`txco dev` brings up the chassis + the `tickets` service and watches both for changes.

In another terminal:

```sh
# Send a fake password-reset request.
swaks --protocol LMTP \
      --server localhost:2424 \
      --from   alice@example.com \
      --to     support@your.tenant \
      --header "Subject: Forgot my password, please help" \
      --body   "Hi, I cannot log in."
# Expect: 250 OK ... queued for triage

# The tickets service logs:
# TICKET {"from":"alice@example.com","to":"support@your.tenant",
#         "subject":"Forgot my password…","category":"account_access",
#         "priority":"high","rid":"…","received_at":"…"}
```

Reject what shouldn't be accepted:

```sh
# Non-support recipient — no rule sets a verdict, default-deny fires.
swaks --protocol LMTP \
      --server localhost:2424 \
      --from   alice@example.com \
      --to     ceo@your.tenant \
      --header "Subject: pls hire me" \
      --body   "..."
# Expect: 550 5.1.1 no rule accepted this recipient
```

Trace it:

```sh
txco trace      # latest envelope: src=lmtp, the classify rule fires,
                # the verdict is visible per recipient
```

## Layout

```
inbound-support-mailbox/
├── README.md                # this file
├── txco.yaml                # one app (tickets), one op (TICKET_CREATE)
├── ingress.yaml             # lmtp recipient → stack
├── APPS/
│   └── tickets/server.js    # toy http server that pretends to mint tickets
└── OPS/
    └── inbound-support/
        ├── 0/accept.txcl              # per-recipient 250 for support@; else 550
        ├── 100/account-access.txcl    # subject → category/priority (highest priority)
        ├── 110/bug-report.txcl         # …guarded so an earlier match wins
        ├── 120/billing.txcl            # …
        ├── 150/uncategorized.txcl      # catch-all default (runs after the arms merge)
        └── 200/post_ticket.txcl        # EXEC "op://TICKET_CREATE"
```

Each `.txcl` resonator holds a single `WHEN` rule. The classify arms are
laddered across scopes (100/110/120) and the lower-priority ones are
guarded on `@ticket.category == ""`, so "first matching wins" is
expressed by scope order — a later scope sees earlier scopes' `EMIT`s,
whereas sibling rules at one scope cannot. The `150` default fires only
when none of the arms matched.

There's no `_sys/` here — `txco dev` scaffolds it on first run.

## Try changing it

- **Add a category.** Drop a new single-rule file beside the others (e.g. `130/feature-request.txcl`) with `WHEN @ticket.category == "" && @lmtp.msg.subject =~ /…/ EMIT @ticket.category = "…"`. Pick a scope between the last arm (120) and the default (150) to set its priority. Hot-reload picks it up.
- **Tempfail on an outage.** Have `200/post_ticket.txcl` return `_txc.lmtp.res.code = 451` on a downstream error so Postfix queues + retries (rather than the chassis's default-deny 550 bouncing the sender).
- **Switch to Strategy A or B routing.** Uncomment the relevant block in `ingress.yaml` (see the inline comments) or set `--lmtp-default-hosts=chassis.example` on the chassis command line for Strategy A. See [`docs/lmtp.md`](../../docs/lmtp.md#resolution-order-per-rcpt-to) for the full routing model.
- **Per-recipient mix.** Send a message to two recipients and accept one / reject the other. One `WHEN` per file, and `EMIT` (not `SET`) so the verdict persists — e.g. add `0/reject-bcc.txcl` next to `0/accept.txcl`:

  ```txcl
  # 0/reject-bcc.txcl
  WHEN @lmtp.rcpt.1 =~ /^bcc-archive@/
    EMIT @lmtp.res.recipients.1.code = 550,
         @lmtp.res.recipients.1.msg  = "bcc archive disabled"
  ```

  Verify with `swaks --to support@your.tenant --to bcc-archive@your.tenant` — the wire log shows two distinct status lines.
- **Front it with Postfix.** Follow [`docs/lmtp.md`](../../docs/lmtp.md#postfix-recipe). The chassis configuration above doesn't change; you just point `mailbox_transport` at the same socket.

## What this example does NOT show

- **DKIM/SPF/DMARC gating.** Postfix would stamp `Authentication-Results`; rules would read it. See `docs/lmtp.md`.
- **Outbound mail.** Inbound only.
- **Per-tenant routing.** Both recipient entries here route to the same `default` tenant. Multi-tenant routing is the same shape — different `tenant:` values per entry.
- **Attachment handling.** Attachments are present on the envelope (`_txc.lmtp.msg.attachments`); this example ignores them. A real ticketing pipeline would forward them to an artifact store and reference them by sha256.
