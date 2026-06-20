# auto-responder — a TxCo package

Inbound email in, an auto-reply out — with the loop protection a real
auto-responder needs. Two rules in the stack's `_mail` channel:

| Scope | File | What it does |
|---|---|---|
| 0 | `_mail/0/accept.txcl` | Accept the recipient (250) so mail lands. |
| 100 | `_mail/100/reply.txcl` | Reply once via `txco://sendmail`, skipping bounces/auto-replies. |

A **package** is an `OPS/`-shaped tree plus a [`txco.package.yaml`](./txco.package.yaml)
manifest. The rules reference no `op://`, so there's **nothing to build and nothing to
wire** — install materializes the files and `txco apply` deploys them. This is the package
the [auto-responder tutorial](../../../docs/tutorial/auto-responder.md) pulls.

## How the mail flows

This stack has no web channel, so it doesn't get a host automatically. After `txco apply`,
mint one and bind it to the stack:

```sh
txco auth tenant hostnames add --mint --stack autoreply
```

That returns a structured host `autoreply-<rand>.stacks.thanks.computer` that is *both*:

- an **inbox** — mail to `<anything>@autoreply-<rand>.stacks.thanks.computer` routes to this
  stack's `_mail` channel, and
- a **verified, DKIM-signing sender** — so the reply (FROM the address that was written to)
  passes the anti-spoof check with no setup.

## Loop protection

Auto-responders that reply to auto-responders create mail storms. The reply rule guards
against it three ways: it skips bounces/DSNs (`@lmtp.is_bounce`), skips anything already
carrying an `Auto-Submitted` header, and uses an at-most-once **campaign** keyed on the
inbound `Message-ID`. The reply itself goes out with `Auto-Submitted: auto-generated` and
a null `<>` reverse-path (RFC 3834).

## Try it

```sh
txco install auto-responder --as autoreply
txco apply
txco auth tenant hostnames add --mint --stack autoreply
# → autoreply-<rand>.stacks.thanks.computer
# email anything@autoreply-<rand>.stacks.thanks.computer → get an auto-reply
```

Customize the `body` in `_mail/100/reply.txcl` (or swap the canned reply for an
[`ai://chat`](../../../docs/ai.md) draft) and `txco apply` again.
