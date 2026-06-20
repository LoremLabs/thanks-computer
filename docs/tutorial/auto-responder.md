# Email auto-responder

_Your second stack: pull a package that replies to email. Deploy it, mint a mail host, send
it a message, get an auto-reply back — all on the hosted service.
([Hello, world](./hello-world.md) first if you haven't.)_

Where [hello, world](./hello-world.md) answered the web, this one answers **email**. You'll
pull the `auto-responder` package — two rules that accept inbound mail and reply once, with
the loop protection a real auto-responder needs — then give it a mail address with a single
command. No inbox to host, no DNS to configure.

:::note
**Cloud or local?** This tutorial uses the hosted service (free tier), where inbound mail
"just works." To run it locally instead, `txco dev --personalities=cron,web,admin,lmtp`
boots a chassis with the mail head and you drive it with `swaks` over LMTP — see the
[inbound-mailbox example](https://github.com/loremlabs/thanks-computer/tree/main/examples/inbound-support-mailbox)
and [lmtp.md](../advanced/protocols/lmtp.md). The cloud path below is the simplest.
:::

## 1. Sign in

If you're continuing from the last tutorial you're already signed in. Otherwise:

```sh
txco login
```

## 2. Pull the package

```sh
mkdir autoresponder && cd autoresponder
txco install auto-responder --as autoreply
```

```
  ✔ verified: signed by txco [SHA256:…]
installed auto-responder 0.1.0 as stack "autoreply/_mail" (2 files)

Review OPS/autoreply/, then run `txco apply` to deploy.
```

`auto-responder` is a **mail** package — its rules live in a `_mail` channel, not the web
scope. `--as autoreply` nests that channel under your chosen stack name, so it lands at
`OPS/autoreply/_mail/`. (`--as` renames the base and keeps the channel; for the web
hello-world package the same flag just renamed the stack.)

Read what you're about to run:

```sh
cat OPS/autoreply/_mail/0/accept.txcl OPS/autoreply/_mail/100/reply.txcl
```

```txcl
# 0/accept.txcl — accept every inbound recipient (250) so mail lands.
WHEN @src == "lmtp"
  EMIT @lmtp.res.code = 250, @lmtp.res.msg = "accepted"

# 100/reply.txcl — reply once, skipping bounces and other auto-replies.
WHEN @lmtp.is_bounce == false && @lmtp.msg.headers.auto-submitted == ""
  SET ._sendmail.to            = @lmtp.mail.from,
      ._sendmail.from          = @lmtp.rcpt.0,
      ._sendmail.subject       = &concat("Re: ", @lmtp.msg.subject),
      ._sendmail.body          = "<p>Thanks for your message! This is an automated reply.</p>",
      ._sendmail.envelope_from = "<>",
      ._sendmail.campaign      = &concat("autoreply:", @lmtp.msg.id)
  EXEC "txco://sendmail"
```

The reply goes **from** the address that was written to (`@lmtp.rcpt.0`) **to** the original
sender (`@lmtp.mail.from`). Three guards stop mail storms: skip bounces (`@lmtp.is_bounce`),
skip anything already `Auto-Submitted`, and an at-most-once `campaign` keyed on the inbound
`Message-ID`. The null `<>` reverse-path is the RFC-3834 auto-reply convention.

## 3. Deploy

```sh
txco apply
```

```
autoreply/_mail v1 activated (2 files)
```

No URL is printed — a mail stack has no web channel, so it doesn't auto-mint a host. You
mint one in the next step.

## 4. Give it a mail address

```sh
txco auth tenant hostnames add --mint --stack autoreply
```

```
minted autoreply-a1b2c3.stacks.thanks.computer → prod-you/autoreply
  routes immediately (verified + DKIM); mail to <anything>@autoreply-a1b2c3.stacks.thanks.computer reaches autoreply/_mail
```

That host is both your **inbox** (mail to it routes to `autoreply/_mail`) and a **verified,
DKIM-signing sender** (so the reply passes the recipient's anti-spoof checks) — minted on
demand, no DNS to set up.

## 5. Email it

From your real inbox, send a message to **any** address at that host:

```
To: hello@autoreply-a1b2c3.stacks.thanks.computer
Subject: testing
```

Within a few seconds you get a reply: *"Thanks for your message! This is an automated
reply."* — from `hello@autoreply-a1b2c3.stacks.thanks.computer`, DKIM-signed.

## Change it

Edit the body (or swap the canned reply for an [`ai://chat`](../ai.md) draft so the answer
is written by a model), then ship again:

```sh
# edit OPS/autoreply/_mail/100/reply.txcl …
txco apply
```

## What you just did

You deployed an email auto-responder to the hosted service: pulled a mail package, applied
it, minted a verified mail host, and replied to real email — with loop protection built in.
From here:

- **[sendmail](../advanced/protocols/sendmail.md)** / **[lmtp](../advanced/protocols/lmtp.md)** — the full outbound + inbound mail contracts.
- **[Packages](../packages.md)** — bundle and publish your own.
- **[AI](../ai.md)** — make the reply a model draft instead of a canned message.
