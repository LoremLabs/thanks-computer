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
| `vars` | Shared template variables; `{{.name}}`-style markers in `subject`/`body` render per recipient (missing keys render empty). Also exposed to the shell as `{{.Vars.name}}` — e.g. a per-send link in a custom `templates.html` |
| `text` | Explicit plaintext part; omitted, it's derived from the HTML body |
| `cc` / `bcc` | Flat address lists added to every message (`cc` visible, `bcc` envelope-only) |
| `reply_to` | Dedicated Reply-To field |
| `headers` | Extra headers map — structural/signing/loop-guard headers are denylisted (use `reply_to`, not a raw header) |
| `envelope_from` | MAIL FROM / Return-Path override. Defaults to `from`. Set `"<>"` for a null reverse-path — the RFC 3834 posture for auto-replies (no bounce loops) |
| `campaign` | Label for rate-limit and audit grouping |
| `retain` | `true` keeps each delivered message's exact bytes (per recipient, as submitted, DKIM-signed) in the content store under this tenant and reports `sha256`/`size` per recipient in the result — the reference a later `txco://imap/append WITH from_sha = …` or `txco://blob/put from_sha` adopts. Off by default; bytes never ride the envelope. Charged per MiB like `blob/put`. Retention never fails a send: a store problem shows as `retain_error` on that recipient. |
| `templates.html` | A custom HTML template to wrap the `body` in, replacing the bundled default. Slots: `{{.Subject}}`, `{{.Body}}`, `{{.Preheader}}`, and `{{.Vars.x}}` for any `vars`. Omitted → the default template |

The HTML body is wrapped in a responsive, CSS-inlined shell — the bundled default, or a
`templates.html` you supply (see below) — and messages are DKIM-signed.

### Custom Template — `templates.html`

By default the `body` is dropped into a built-in responsive shell. To control the whole
email (your own header, footer, branding, responsive `@media` styles), pass a complete
HTML document as `templates.html` with the same `{{.Body}}` / `{{.Subject}}` /
`{{.Preheader}}` slots the default uses (plus `{{.Vars.x}}` for any per-send `vars`,
e.g. a `{{.Vars.nexturl}}` button href):

```txcl
# load a template shipped in the stack's FILES, then send with it
WITH files = &array(&object("path", "_email/drip.html", "as", "tmpl"))
EXEC "txco://read-file"

SET ._sendmail.to            = .reader,
    ._sendmail.from          = "news@ops.example.com",
    ._sendmail.subject       = "This week",
    ._sendmail.body          = .article_html,
    ._sendmail.templates.html = ._files.tmpl.content
EXEC "txco://sendmail"
```

Variable substitutions are possible inside a `html/template`: `{{.Subject}}` is auto-escaped, `{{.Body}}` is your
`_sendmail.body`. 

:::note
The `from` domain must be a **verified hostname of the sending tenant**
([routing](../../routing.md#how-the-two-sources-compose)) — a rule can't send as a
domain its tenant doesn't own. If a send fails with `from_not_verified`, add and
verify the hostname first (`txco auth tenant hostnames add … && … verify`).
:::

## What comes back

The `EXEC` of the operation merges a result under `_sendmail.result`:

- success: `{sent, skipped, failed, recipients: […]}` — per-recipient
  outcomes `{to, status, message_id?, reason?, sha256?, size?, retain_error?}`;
  rate-limited recipients are skipped with reason `rate_limited`;
  `sha256`/`size` appear only with `retain = true`
- error: `{status: "error", reason, error}` — reasons include
  `no_relay`, `missing_field`, `invalid_from`, `from_not_verified`,
  `no_recipients`, `too_many_recipients`, `invalid_template`

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
