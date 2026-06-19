# Email Out — Send email with `EXEC txco://sendmail`

You can send email from [Thanks, Computer](https://www.thanks.computer) as well as [receive it](./lmtp.md). 

To send:

-  Configure the `_sendmail` contract on the envelope 
-  `EXEC "txco://sendmail"` to request the email be sent.

```txcl
WHEN .resolution == "credited"
SET ._sendmail.subject = "Your credit has been applied",
    ._sendmail.from    = "billing@ops.example.com",
    ._sendmail.to      = .customer.email,
    ._sendmail.body    = "<p>Hi {{.name}}, your credit for invoice {{.invoice}} is in.</p>"
EXEC "txco://sendmail"
```

## The `_sendmail` contract

Required: `subject`, `body` (HTML), `from`.

| Field | Meaning |
|---|---|
| `to` | One address, a list, or a list of `{address, vars}` objects for per-recipient personalization |
| `vars` | Shared template variables; `{{.name}}`-style markers in `subject`/`body` render per recipient (missing keys render empty) |
| `text` | Explicit plaintext part; omitted, it's derived from the HTML body |
| `cc` / `bcc` | Flat address lists added to every message (`cc` visible, `bcc` envelope-only) |
| `reply_to` | Dedicated Reply-To field |
| `headers` | Extra headers map — structural/signing/loop-guard headers are denylisted (use `reply_to`, not a raw header) |
| `envelope_from` | MAIL FROM / Return-Path override. Defaults to `from`. Set `"<>"` for a null reverse-path — the RFC 3834 posture for auto-replies (no bounce loops) |
| `campaign` | Label for rate-limit and audit grouping |

The HTML body is wrapped in a responsive, CSS-inlined default shell, and messages are
DKIM-signed.

**Anti-spoof:** the `from` domain must be a *verified hostname of the
sending tenant* ([ingress.md](../../routing.md#how-the-two-sources-compose)) —
a rule cannot send as a domain its tenant doesn't own.

## What comes back

The `EXEC` of the operation merges a result under `_sendmail.result`:

- success: `{sent, skipped, failed, recipients: […]}` — per-recipient
  outcomes; rate-limited recipients are skipped with reason
  `rate_limited`
- error: `{status: "error", reason, error}` — reasons include
  `no_relay`, `missing_field`, `invalid_from`, `from_not_verified`,
  `no_recipients`, `too_many_recipients`

## Operator configuration

Sending is **off until a relay is configured**. The chassis is a
submitter, not an MTA: it hands rendered messages to your edge SMTP.

| Flag | Default | Meaning |
|---|---|---|
| `--mail-relay-addr` | _(empty = disabled)_ | SMTP submission address (`host:port`) |
| `--mail-relay-tls` | `none` | `none` (trusted private net) or `starttls` |
| `--mail-dial-timeout-ms` | `5000` | Dial + submit deadline; a down relay fails fast |
| `--mail-max-recipients` | `50` | Per-call cap; over it the op errors rather than truncating |
| `--mail-rate-limits` | _(empty = off)_ | Per-tenant caps, e.g. `"100/2m,200/4h"` — every rule must be under its cap. **Per node**, in memory: a runaway-loop valve, not fleet-wide accounting |
