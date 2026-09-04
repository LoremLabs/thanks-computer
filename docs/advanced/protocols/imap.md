<!-- nav: IMAP -->

# IMAP — A mail client as the UI

_The `imap` personality serves a durable mailbox store to any IMAP client —
Apple Mail, Thunderbird, mutt. Stacks put messages into it with an op; the
client reads them. The head is a generic IMAP server: it knows no folder
names and no product._

Nothing lands in a mailbox on its own. Inbound mail is parsed onto the
envelope and forgotten, exactly as before; a rule that wants a message to
show up in a client calls `txco://imap/append` with the **record** it chose
to keep — a headers subset, the normalized body, attachment references —
and the head renders RFC 5322 from that record when a client fetches it.
The wire form is derived; the record is what is retained.

The other direction is a stack **hearing** what the client did: every
committed mutation (a drag into a folder, a new folder, a delete) can
reach the tenant's `_imap` stack after the fact, and a folder can be set
to ask the stack first. That is how "any folder the owner drops a PDF
into becomes knowledge" is written in txcl, not in Go.

## Turn it on

```
txco serve --personalities cron,web,admin,imap \
  --imap-listen-addrs :1143            # plaintext; or
  --imap-tls-addrs :993 --imap-hostname imap.example.com
```

Both gates must be flipped: `imap` in `--personalities` **and** at least one
listen list non-empty. The index lives in its own SQLite file
(`--imap-db-path`, default `./chassis/data/imap.db`); message bytes go to
the content store the blob plane uses.

On a fleet, `--imap-store` selects a backend an overlay registers (the
hosted build ships `postgres`, reading `TXCO_DB_AUTH_DSN`). A shared backend
is opened on **every** node, head or not, so `txco://imap/*` on any node
project into the one index the head serves; a node whose open failed at
boot answers `txco_imap_disabled` until restart. The head keeps one
snapshot per selected mailbox and diffs it against the index by `modseq`,
so a client's next command always sees the latest index and, during IDLE,
changes made on other nodes arrive within `--imap-sync-interval`.

For local development:

```
txco dev --imap        # 127.0.0.1:1143 (STARTTLS) + 127.0.0.1:1993 (IMAPS), self-signed certificate
```

Desktop mail clients will not even attempt LOGIN over a plaintext port, so
the dev head serves a self-signed certificate (`--imap-self-signed`)
covering loopback and the dev-local hostname patterns, kept at
`.txco/dev/imap-selfsigned.crt` so it is the same one on every restart.
Trust it once in the login keychain and clients connect without a prompt:

```
security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db .txco/dev/imap-selfsigned.crt
```

Then add the account with **SSL on, port 1993**. `TXCO_IMAP_WIRE_DEBUG=true`
logs every command and response when a client misbehaves, and every LOGIN
attempt logs one `imap login` line with its outcome.

## Accounts: `txco://imap/account`

```txcl
EXEC "txco://imap/account"
  WITH username = "paris@pony.example.com",   # <local>@<domain the tenant owns>
       password = "",                         # "" (or omitted on create) generates one
       into = "_acct"
```

| WITH | Meaning |
|---|---|
| `username` (req) | `<local>@<domain>`. The domain must be a verified hostname binding or a delegated DNS zone of the tenant — the same ownership rule `txco://sendmail` applies to `From:`. Usernames are global: one tenant per address. |
| `password` | Omitted: unchanged on update, generated on create. `""`: generated. Otherwise stored (≥ 8 chars). Only an argon2id hash is kept. |
| `rotate` | `true`: generate a new password for an existing account and return it once (a "rotate mailbox password" button). With an explicit `password` it is simply that password. |
| `password_style` | How a generated password looks: `token` (default) is a 24-character group token (`xxxx-xxxx-…`, ~116 bits); `words` is hyphen-joined words from the BIP-39 list (`river-galaxy-bamboo-orbit-velvet`), 11 bits per word — the phrase a person types into a phone. |
| `password_words` | Word count for `words`, 4–12, default 5 (55 bits: online guessing is throttled by the head, and an offline attack on the argon2id hash is measured in millennia). |
| `status` | `active` (default) or `disabled`. |
| `policy` | Account-default policy object (reserved for the next phase). |

Result at `into` (default `_imap`): `{username, created, password?, rotated?}` —
`password` appears **only** when it was generated (create, `""`, or
`rotate`), and only this once; `rotated` is `true` when an existing
account's password was regenerated. Creating an account creates its `INBOX`.

## Messages: `txco://imap/append`

```txcl
EXEC "txco://imap/append"
  WITH username = "paris@pony.example.com",
       mailbox = "INBOX",                     # a name, or "role:<role>"
       object_key = "msg:" + .msgkey,         # your stable identity for this message
       message = &object(
         "from", "Paris <paris@pony.example.com>",
         "to", "owner@example.com",
         "subject", .subject,
         "text", .normalized_text),
       flags = &array("$Processed"),
       into = "_appended"
```

| WITH | Meaning |
|---|---|
| `username`, `mailbox` | The account (must belong to this tenant) and the target mailbox (default `INBOX`). |
| `object_key` (req) | Your key. Same key + same content → `noop`; same key + different content → the old UID is expunged and a **new** UID allocated (`replaced`). Bytes under a UID never change. |
| `message` | `{from, to, cc, reply_to, subject, date, message_id, in_reply_to, references, headers{}, text, html, attachments[{name, content_type, size, sha256?}]}`. Attachments are references: name/type/size always, a `sha256` only when the bytes are in the content store. |
| `flags` | Initial flags/keywords (`\Seen`, `$Anything`). |
| `internaldate` | RFC3339; default now. Also the `Date:` fallback. |

Result: `{uid, uidvalidity, sha256, size, noop, replaced, mailbox}`.
Errors land as `<into>.error.{code, message}` with the run continuing:
`txco_imap_disabled`, `txco_imap_no_account`, `txco_imap_no_mailbox`,
`txco_imap_domain_not_owned`, `txco_imap_username_taken`,
`txco_imap_invalid_arg`, `txco_imap_too_large`, `txco_imap_unsupported`
(verbatim `from` / `from_sha` appends are not available yet).

The op charges [fuel](../fuel.md) per MiB of the rendered message like
`blob/put`. Missing `Date:` and `Message-ID:` are synthesized
deterministically (from the internal date and the object key) so a
message renders byte-identically on every fetch.

## The rest of the op family

| Op | WITH | Returns |
|---|---|---|
| `txco://imap/mailbox` | `username`, `name` (full path) or `id`, `role`, `attrs[]` (special-use), `policy{}`, `rename_to`, `delete`, `reset` | `{id, name, role, attrs, policy, uidvalidity, created, deleted?}` — creates when absent, updates otherwise |
| `txco://imap/remove` | `username`, `mailbox`, `uid` or `object_key` | `{removed, uid}` |
| `txco://imap/flags` | `username`, `mailbox`, `uid` or `object_key`, `add[]`, `remove[]` | `{uid, flags[]}` |
| `txco://imap/list` | `username`, `prefix` | `{mailboxes:[{id, name, role, attrs, policy, uidvalidity, messages, unseen}], count}` |
| `txco://imap/messages` | `username`, `mailbox`, `after` (uid cursor), `limit` (≤ 1000), `flags[]` (any-of) | `{items:[{uid, object_key, kind, sha256, size, internaldate, flags, subject, from, parts[]}], next, count}` |
| `txco://imap/get` | `username`, `mailbox`, `uid` or `object_key`, `raw` | `{…row, headers{}, text, html, parts[], raw? (base64)}` |

`txco://imap/append` also takes **verbatim** bytes instead of `message{}`:
`from` (an envelope path holding base64 RFC 5322, e.g. `@lmtp.msg.raw`) or
`from_sha` (an RFC 5322 object the tenant already owns in the content
store). The exact bytes are retained and every decoded attachment becomes
its own owned object. `txco://blob/put` gained `from_sha` too: name an
owned object (a part the head stored) under your own scheme without
re-uploading it.

`mailbox` in any op is a name (default `INBOX`) or `role:<role>` — the
stable key a stack set, which survives a client RENAME.

## What a client gets

`LOGIN` (per-IP and per-account throttles, a suspended tenant is refused),
`NAMESPACE`, `LIST` (implicit parents as `\Noselect`, `CHILDREN`,
special-use), `STATUS`, `SELECT`, `FETCH` (ENVELOPE, BODYSTRUCTURE,
RFC822.SIZE, FLAGS, BODY[…]), `SEARCH` (UID/sequence, flags, dates, size,
Subject/From/To/Cc/Bcc/Message-ID, BODY/TEXT over the stored text),
`STORE`, `CREATE` (nested, `CREATE-SPECIAL-USE`), `DELETE`, `RENAME`
(subtree), `APPEND` (verbatim, `APPENDLIMIT`), `COPY`, `MOVE`, `EXPUNGE`,
`SUBSCRIBE`, `NOOP`/`IDLE` — a rule's append shows up in an open client
as `EXISTS`: at once when the op ran on this node, within
`--imap-sync-interval` (15s) when it ran on another node against a shared
index. An operator reset of a selected mailbox (a new UIDVALIDITY) ends
the session with `BYE`; the client reconnects and reselects. Bytes under a
UID never change; a client APPEND of the same
message twice is two UIDs (no `object_key`), a stack append with the same
key is a no-op or a replacement.

## Policy: what a stack hears, and when

Per mailbox, seven verbs — `append`, `move_in`, `move_out`, `delete`,
`flags`, `create` (of children), `rename` — each one of:

| Mode | Effect |
|---|---|
| `deny` | `NO [NOPERM]` at the protocol layer, no round trip |
| `local` | protocol state only, no event (the default for `flags`) |
| `observe` | commit, then tell the `_imap` stack, fire-and-forget (the default for everything else) |
| `stack` | ask the `_imap` stack **first**; `NO` unless it answers `@imap.res.ok = true` |

Resolution: the mailbox's `policy`, then the account's `policy` (the
default for client-created folders), then the chassis default. A MOVE
consults `move_out` on the source and `move_in` on the destination and
takes the stricter.

The `_imap` stack is the subscription — while it exists, events flow;
without it `observe` is silent and `stack` answers `NO [UNAVAILABLE]`.

### The envelope

```
@src            "imap"                       @client.ip
@imap.tenant    (slug)                        @imap.account   (username)
@imap.phase     observe | answer              @imap.op        append | copy | move | expunge | flags | create | delete | rename
@imap.mailbox   {id, name, role}              @imap.dest      {id, name, role}   (copy/move/create/rename)
@imap.uid       (append: the allocated UID)   @imap.objects[] {uid, object_key, sha256, flags}
@imap.msg       {id, date, subject, from[], to[], cc[], headers{}, text, html, sha256, size,
                 parts[{n, name, type, size, sha256}]}                             (append only; no bytes)
```

Bytes never ride the envelope: the message and each part are in the
content store before dispatch, addressable by `sha256` through
`txco://blob/get` (or adopted by name with `blob/put from_sha`). A rule may
`EMIT @delete = &array("@imap.msg.text", "@imap.msg.html", "@imap.msg.headers")`
once it has consumed them.

### Answering (`@imap.phase == "answer"`)

```txcl
WHEN @imap.phase == "answer" && @imap.mailbox.role == "readonly"
  EMIT @imap.res.ok = false, @imap.res.code = "cannot", @imap.res.msg = "read-only"

WHEN @imap.phase == "answer" && @imap.op == "append" && @imap.mailbox.role == "knowledge"
  EMIT @imap.res.ok = true, @imap.res.flags = &array("$Ingested"), @imap.res.object_key = "docs/" + @imap.msg.parts.0.sha256
```

`ok` absent or false is a `NO`; `code` is `cannot`, `limit` or
`unavailable` (RFC 5530); `msg` is shown to the client; on an append,
`flags[]` and `object_key` land on the stored row. The head waits
`--imap-resp-timeout` (30 s); past it the client gets `NO [UNAVAILABLE]`
and a late answer is discarded. A continuation suspend counts as ok —
the run is durable and keeps working on the message.

Observe-lane runs are sampled and bounded (`--imap-observe-sample`,
`--imap-observe-max-inflight`); a full queue drops the observation rather
than delay a client. Both lanes meter fuel like any run.

Mail client settings: IMAP server = the chassis host, port `1993` (dev,
self-signed) or `993` (prod) with SSL on, username = the full address,
password = the generated one. Apple Mail verifies an outgoing server too
when adding an account; there is no submission head yet, so for a local
test run Mailpit (`mailpit --smtp-auth-accept-any --smtp-auth-allow-insecure`)
and point outgoing at `localhost:1025`, or use a real SMTP account.

## Mail client settings

The head never routes by name: a connection only needs a certificate to
present, and the front proxy serves whichever one it holds for the name
the client asked for. So on a fleet where every stack has its own hostname
(and its own certificate), **the server a mail client should use is the
domain of the address** — `paris@<stack>.stacks.example` connects to
`<stack>.stacks.example`, port 993, SSL on. There is no `imap.` name.
Apple Mail guesses `imap.`/`mail.` and has to be told the server. Thunderbird
fetches `https://<domain>/.well-known/autoconfig/mail/config-v1.1.xml`
before it guesses; a stack answers that with two short rules
(`examples/imap-hello`, `OPS/imap-demo/080` and `085`) and a Thunderbird
user then types only the address and password. Thunderbird does not
consult RFC 6186 `_imaps._tcp` SRV records; the `dns` personality can still
publish them for clients that do (`--dns-imaps-port`, see [dns](./dns.md)).

On the hosted fleet that is exactly the topology: the edge terminates
`:993` with the stack's existing certificate and forwards the session to
the head over the private network with the PROXY protocol, the same path
the stack's HTTP takes.

## TLS

Three ways to hold the certificate, all supported:

- **A front proxy terminates** (recommended for a fleet): Caddy `layer4`,
  nginx `stream`, haproxy on `:993`, forwarding plaintext to
  `--imap-listen-addrs` bound on a private interface, with the PROXY
  protocol (`--imap-proxy-protocol <proxy cidr>`) so throttles and the
  envelope see the real client address.
- **The chassis terminates with the bundled cert manager**:
  `--imap-tls-addrs :993 --imap-hostname imap.example.com` with the `dns`
  personality serving the zone (ACME DNS-01). Works without
  `--web-tls-addr`.
- **Cert files**: `--imap-tls-cert-file` / `--imap-tls-key-file` from
  certbot or a same-host Caddy.

`--imap-insecure-auth` permits `LOGIN` on a plaintext connection; keep it
for `txco dev` and behind-a-proxy deployments only.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--imap-listen-addrs` | (empty) | Plaintext listeners |
| `--imap-tls-addrs` | (empty) | Implicit-TLS listeners |
| `--imap-hostname` | (empty) | Public name, added to the managed certificates |
| `--imap-tls-cert-file` / `--imap-tls-key-file` | (empty) | Certificate from elsewhere |
| `--imap-proxy-protocol` | (empty) | CIDRs of front proxies whose PROXY v1/v2 header is honoured (the real client IP) |
| `--imap-store` / `--imap-db-path` | `sqlite` / `./chassis/data/imap.db` | The index; a non-sqlite backend is shared and opened on every node |
| `--imap-sync-interval` | `15s` | How often selected mailboxes are re-read for changes from other nodes (0 = next command only) |
| `--imap-insecure-auth` | `false` | LOGIN without TLS |
| `--imap-self-signed` | `false` | Mint a self-signed certificate at boot (dev; `txco dev --imap` sets it) |
| `--imap-wire-debug` | `false` | Log every IMAP line at DEBUG, credentials included (dev) |
| `--imap-login-rate` | `10` | LOGIN attempts per minute, per IP and per username |
| `--imap-max-conns-per-account` | `16` | Simultaneous authenticated connections |
| `--imap-append-max-bytes` | 32 MiB | Size cap for `txco://imap/append` and a client APPEND (`APPENDLIMIT`) |
| `--imap-resp-timeout` | `30s` | Answer-lane deadline |
| `--imap-observe-sample` / `--imap-observe-max-inflight` | `1` / `8` | Observe-lane sampling and concurrency |
| `--imap-proxy-protocol` | (empty) | CIDRs allowed to send a PROXY (v1/v2) header — the real client IP behind a front proxy |

Env: `TXCO_IMAP_*`. Example: `examples/imap-hello`.
