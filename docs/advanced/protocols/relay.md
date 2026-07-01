# Mail forwarding — re-send inbound mail with `EXEC txco://relay`

Where [`txco://sendmail`](./sendmail.md) *composes* a new email, `txco://relay` **forwards
an inbound message verbatim** — the Unix `.forward` primitive. It re-sends the original
RFC 5322 bytes untouched (the sender's `From:`, body, and attachments all survive), only
rewriting the SMTP envelope. Because the message isn't altered, the sender's original DKIM
signature stays valid, so DMARC still aligns to their domain.

Use it in an [`_mail`](./lmtp.md) stack to point a static address at a real inbox:

```txcl
# Forward matt@ and hello@ to a real mailbox, like a .forward file. Runs BEFORE the
# publication existence check, guarded against loops, and halts (a forward is terminal).
WHEN @lmtp.rcpt.0 =~ /(?i)^(matt|hello)@example\.com$/ && @lmtp.is_bounce == false && @lmtp.msg.headers.auto-submitted == ""
  SET ._relay.to            = "someone@gmail.com",
      ._relay.raw           = @lmtp.msg.raw,
      ._relay.envelope_from = "noreply@example.com",
      ._relay.campaign      = &concat("fwd:", @lmtp.msg.id)
  EXEC "txco://relay"
  EMIT @lmtp.res.code = 250, @lmtp.res.msg = "accepted", @halt = true
```

## The `_relay` contract

| Field | Required | Notes |
| --- | --- | --- |
| `raw` | **yes** | The message to send, base64 RFC 5322 — normally `@lmtp.msg.raw` (the full inbound message the LMTP inlet retains). Sent **byte-for-byte unchanged** — no re-compose, no re-sign. |
| `to` | **yes** | The forward recipient (a single address). |
| `envelope_from` | **yes** | The SMTP `MAIL FROM` / Return-Path to stamp. Its **domain must be verified for the tenant** (the same anti-spoof check `sendmail` runs on `from`). Bounces go here. |
| `campaign` | no | At-most-once dedup key (e.g. `"fwd:" + @lmtp.msg.id`) so an LMTP redelivery doesn't forward twice. |

Result lands under `._relay.result` (`{status: "sent" | "skipped" | "error", to,
envelope_from, reason?}`).

## Deliverability

A verbatim forward is a *good* forward, not a broken one:

- **DKIM survives → DMARC passes.** The original signature is over the original bytes; we
  don't touch them, so it still validates and DMARC aligns to the sender's domain. (This is
  precisely what DKIM-survives-forwarding is designed for.)
- **SPF aligns via `envelope_from`.** Stamping a Return-Path on a domain you own (e.g.
  `noreply@example.com`) makes SPF pass at the receiver for *your* domain — avoiding the
  classic `.forward` SPF break (where keeping the sender's envelope-from fails SPF). Bounces
  land on that address; pair it with a `noreply@` sink that eats them.
- **Loop guards.** Never forward a bounce or an auto-response — gate on
  `@lmtp.is_bounce == false && @lmtp.msg.headers.auto-submitted == ""`.

## Security — relay is inbound-mail only

`txco://relay` ships **arbitrary bytes** out under a verified return-path, so it is more
privileged than `sendmail` (which forces a verified `From:` and DKIM-signs what it builds).
To prevent it being driven into a spam/phishing relay by some *other* pipeline (a coerced
web endpoint feeding attacker-supplied `_relay.raw`), the op **refuses unless the request
originated from the LMTP inlet.** That check reads the request's source pinned in trusted
context at ingress — not the mutable `_txc.src` envelope field a rule could forge. A
`_relay` assembled by an HTTP/cron/etc. pipeline is rejected before anything is sent.

Attachments and exact MIME are preserved (unlike `sendmail`, which has no attachment
support). Relay pays normal [fuel](../fuel.md) and appears in [traces](../trace.md).
