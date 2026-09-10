# Remote sources — watch a mailbox you already own

_Where [receiving email](./lmtp.md) needs mail delivered TO the chassis (an MX
or a forward), a **source** connects OUT and reads a mailbox you already have —
a support address at Fastmail, a Gmail account, a self-hosted Dovecot. Each new
message becomes one run of your `_source` stack, and once that run succeeds the
message is moved aside or marked read at the source._

A source is a **durable subscription**, not an op: you declare it once, and the
chassis polls it on an interval, remembers how far it has read (a cursor), and
coordinates so exactly one node in a fleet polls each mailbox. It is the natural
way to delegate a mailbox to a stack without touching the provider's MX or
forwarding.

## Declare a source

Sources live in a stack's `OPS/<stack>/SOURCES/<pack>.jsonl` — one JSON object
per line — and are applied with `txco apply`, exactly like your ops. The line
DECLARES the source; the credential is referenced by **name** and never appears
in the file:

```jsonl
{"id":"support","kind":"imap","host":"imap.fastmail.com","port":993,"tls":"implicit","user":"desk@acme.com","secret":"ACME_DESK_MAILBOX","mailbox":"INBOX","every":"5m","on_processed":"move:Processed"}
```

| field | meaning |
|---|---|
| `id` | stable name for this source within the pack (the cursor is keyed to it) |
| `kind` | `imap` (the only kind today) |
| `host` / `port` | the IMAP server; `port` defaults to 993 (implicit TLS) or 143 |
| `tls` | `implicit` (default, port 993), `starttls` (port 143), or `none` (dev only) |
| `user` | the login username |
| `secret` | the NAME of a [tenant secret](../runbook-secret-store.md) holding the password |
| `mailbox` | the folder to watch (default `INBOX`) |
| `every` | poll interval — `"5m"`, `"90s"`, `"1h"`, or a number of seconds (default 300) |
| `on_processed` | what to do once a message's run succeeds: `move:<folder>`, `seen`, or `none` |
| `enabled` | set `false` to switch a source off without removing it |

Store the password once, by name, before you apply — it is encrypted at rest and
never leaves the secret store:

```
txco auth tenant secrets set ACME_DESK_MAILBOX --tenant acme
```

## Receive the runs

Each new message fires into your stack's **`<stack>/_source/0`** — a nested
channel stack beside your app, like `<stack>/_mail`. Define one to receive them
(its existence is the subscription). The message arrives in the same shape as an
LMTP-delivered one, under `@source.msg`:

```txcl
# OPS/support-desk/_source/0/triage.txcl
WHEN @source.msg.subject != ""
EXEC "txco://route" WITH stack = "triage"
```

| field | meaning |
|---|---|
| `@source.msg.*` | the parsed message: `subject`, `text`, `html`, `from[].addr`, `headers.*`, `attachments[]`, `raw` (b64 original) — identical to `@lmtp.msg.*` |
| `@source.id` | the source's declared id |
| `@source.key` | a stable dedup id for this message (`<uidvalidity>:<uid>`) |
| `@source.stack` / `@source.tenant` | the declaring stack / tenant |
| `@source.meta.uid` / `@source.meta.flags` | IMAP specifics |

**Delivery is at-least-once.** If a node crashes between a successful run and the
move, the message is read again next poll. Guard against acting twice on the
same message with `@source.key` — for example a `txco://kv/cas` on it, or
`txco://imap/append` with `object_key = @source.key`, which dedups by design.

### Overriding the disposition per message

`on_processed` is the default action. A run can override it for one message by
emitting `_txc.source.res.action` — so a stack can move a handled message to one
folder and a rejected one to another:

```txcl
WHEN @source.msg.spam == true
EMIT @source.res.action = "move:Rejected"
```

Valid actions are `move:<folder>`, `seen`, and `none`.

## What it does and does not touch

- **Read + move/archive only.** A source FETCHes and, on success, MOVEs or marks
  `\Seen`. It never deletes or expunges. If the server does not support `MOVE`, a
  `move` degrades to marking `\Seen` (logged) rather than emulating a move with a
  copy-then-delete that could lose mail.
- **The dial is egress-guarded.** A source obeys `--egress-policy`; with the
  default `private` policy it will refuse to connect to loopback or private
  address space. To watch a mailbox on your own network, add its range to
  `--egress-allow-cidrs` — a refusal there is the guard, not a connection error.
- **App passwords only, for now.** Use a per-mailbox app password (Fastmail,
  Google Workspace, Dovecot). OAuth2 / XOAUTH2 (Gmail/M365 consent) is a later
  addition.

## Operate

`txco source status` shows each declared source with its runtime state — the
cursor (how far it has read), which node holds the poll claim, when it next
polls, and the last error if any. It never shows a secret value.

```
txco source status --tenant acme
```

## Run the head

The source poller is off by default. Turn it on where you want mailboxes polled
(a data-plane node, like the scheduled inlet):

- add `source` to `--personalities`
- the mailbox passwords live in the tenant secret store (`--secret-master-key`
  must be configured)

Across a fleet, the source store is the shared runtime database, so the poll
**claim** coordinates nodes: several nodes may run the `source` personality and
each mailbox is still polled by exactly one of them per cycle. With a node-local
SQLite runtime DB, run the poller on a single node only.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--source-period` | `30` | Seconds between poll passes |
| `--source-max-inflight` | `8` | Max source connections opened per pass |
| `--source-batch` | `50` | Max messages one source yields per pass (a big first sync drains over several passes) |
| `--source-stale-after` | `600` | Seconds before a crashed node's claim is reclaimed |
| `--source-dispatch-timeout` | `60` | Seconds to wait for one message's run before treating it as failed |
