<!-- nav: Contacts -->

# Contacts — CardDAV as the UI

_The `contacts` personality serves a durable contacts store to any CardDAV
client — Apple Contacts, iOS, Thunderbird, DAVx⁵. Stacks put cards into it
with an op; the client reads them. The head is a generic address book
server: it knows no product and never runs a stack to answer a read._

Nothing lands in an address book on its own. A rule that wants a person to
show up in a contacts app calls `txco://contacts/put` with the **card** it
chose to publish — a structured object the chassis renders to vCard 3.0, or
the vCard text itself — and the head serves it from the store.

The other direction is a stack **hearing** what the client did: every
committed mutation (a card added, edited, deleted) can reach the tenant's
`_contacts` stack after the fact, and an address book can be set to ask the
stack first — and to accept the stack's rewrite of the client's card. That
is how "add a card and the pony lets that person write to it" is written
in txcl, not in Go.

The [calendar](./calendar.md) personality is this one's sibling: same
account model, same policy vocabulary, same lanes, same pack kind. What
differs is the object — and one consequence of it: a contacts app expects
to read back **exactly the bytes it wrote** (photos, `X-` properties,
groups), so the head stores a client's card as written and serves it back
verbatim, rather than re-encoding it.

## Turn it on

```
txco serve --personalities cron,web,admin,contacts
```

There is no listener of its own: the head mounts on the **web head** under
a reserved path prefix on every hostname it serves —
`--contacts-path-prefix` (default `/carddav`) plus `/.well-known/carddav`,
which redirects into it. It must differ from `--calendar-path-prefix`
(the chassis refuses to start otherwise). TLS is the web head's. The index
lives in its own SQLite file (`--contacts-db-path`, default
`./chassis/data/contacts.db`); cards live in the index, never the blob CAS.

On a fleet, `--contacts-store` selects a backend an overlay registers (the
hosted build ships `postgres`, reading `TXCO_DB_AUTH_DSN`). A shared
backend is opened on **every** node, head or not, so `txco://contacts/*` on
any node project into the one index the head serves.

For local development:

```
txco dev --contacts       # http://<dev host>:<web port>/carddav/, Basic auth over plaintext
```

Add the account in Contacts (macOS) with **Add Account → Other Contacts
Account → CardDAV → Advanced**: server = the bound host, port = the web
port, SSL off, path `/carddav/`; in Thunderbird, **Address Book → New
CardDAV Address Book** with the same server and username. Every Basic-auth
attempt logs one `contacts login` line with its outcome.

## Accounts: `txco://contacts/account`

```txcl
EXEC "txco://contacts/account"
  WITH username = "paris@pony.example.com",   # <local>@<domain the tenant owns>
       password = ._imapacct.password,        # the same password the IMAP account got
       into = "_conacct"
```

The WITH clause is `txco://calendar/account`'s: `username` (req; the
domain must be a verified hostname or delegated zone of the tenant),
`password` (omitted: unchanged on update / generated on create; `""`:
generated; else stored, ≥ 8 chars), `rotate`, `password_style` /
`password_words`, `status`, `policy` (the account-default mutation policy).
Result at `into` (default `_contacts`): `{username, created, principal,
password?, rotated?}` — `password` appears **only** when generated, once.
Pass the password another head minted and one credential opens Mail,
Calendar and Contacts.

## Address books: `txco://contacts/addressbook`

```txcl
EXEC "txco://contacts/addressbook"
  WITH username     = "paris@pony.example.com",
       name         = "senders",
       display_name = "Paris senders",
       description  = "Who may write to this pony.",
       policy       = &object("put", "stack", "delete", "stack"),
       into         = "_book"
```

| WITH | Meaning |
|---|---|
| `username`, `name` (req) | The account and the book's path segment (`[A-Za-z0-9._~-]`, up to 128 chars). Creates when absent, updates otherwise; empty fields are left alone. |
| `display_name`, `description`, `sort_order` | What the client shows. |
| `policy` | Per-book mutation policy (below). |
| `remove` | `true`: soft-delete the book and tombstone its objects. |

Result: `{id, name, path, display_name, description, sort_order, policy,
sync_token, created}` or `{name, removed}`.

## Cards: `txco://contacts/put`

```txcl
EXEC "txco://contacts/put"
  WITH username    = "paris@pony.example.com",
       addressbook = "senders",
       name        = "bob-example.com.vcf",     # the resource name on create
       card        = &object(
         "fn",     "Bob Example",
         "emails", &array(&object("value", "bob@example.com"))),
       into        = "_put"
```

| WITH | Meaning |
|---|---|
| `username`, `addressbook` | The account and the book (its `name`). |
| `card{}` **or** `vcard` | The structured card (the chassis renders vCard 3.0) or vCard text (kept as written; the UID is the bytes' own). Not both. |
| `uid` | The object's identity. Optional with `card{}`: derived from `name` as `<name>.<local>@<domain>`, stable across re-materializations. |
| `name` | Resource name on create; on update the object keeps the name it has, whatever the client chose. |

`card{}` is generic vCard vocabulary: `uid`, `fn` (defaults to the first
email), `name{family, given, additional, prefix, suffix}`, `nickname`,
`org`, `title`, `note`, `url`, `birthday` (`YYYY-MM-DD`), `emails[{value,
type[], pref}]`, `phones[{value, type[], pref}]`, `kind` (`individual`,
`group`, `org`, `location`) and, for a group, `members[]`. vCard text must
be one VCARD, version 3.0 or 4.0, with a UID.

Result: `{name, path, uid, etag, created, noop, modseq}`. The same content
again (REV and PRODID aside) is a `noop` — a re-materialization never
changes an etag. The op charges [fuel](../fuel.md) per MiB like
`blob/put`. Errors land as `<into>.error.{code, message}` with the run
continuing: `txco_contacts_disabled`, `txco_contacts_no_account`,
`txco_contacts_no_addressbook`, `txco_contacts_no_object`,
`txco_contacts_domain_not_owned`, `txco_contacts_username_taken`,
`txco_contacts_invalid_arg`, `txco_contacts_too_large`,
`txco_contacts_conflict` (the UID names another resource).

## Lists: `txco://contacts/sync`

A stack has no loops, and an address book often mirrors a **list** — the
people allowed to write, a team, a roster. `sync` applies a bounded batch
in one transaction:

```txcl
EXEC "txco://contacts/sync"
  WITH username    = "paris@pony.example.com",
       addressbook = "senders",
       put         = ._plan.put,       # [{name?, uid?, card{} | vcard}, …]  ≤ 200
       delete      = ._plan.delete,    # [uid, …]                            ≤ 200
       into        = "_sync"
```

Every `put` entry is a `txco://contacts/put` body, addressed by UID; every
`delete` is a UID (an unknown one counts as `missing`, not an error). Any
invalid entry refuses the whole batch and names it (`error.index`,
`error.op`); nothing is written. Result: `{created, updated, noop, deleted,
missing, count, items[]}`. The usual shape is `list` → a compute decides
what to add and drop → `sync`.

## The rest of the op family

| Op | WITH | Returns |
|---|---|---|
| `txco://contacts/get` | `username`, `addressbook`, `uid` or `name` | `{name, path, uid, etag, size, version, fn, kind, addresses[], modseq, updated_at, vcard, card{}}` — `card` is the parse: every input field plus `version`, `rev`, `addresses[]` (the EMAIL values, lowercased, deduped) |
| `txco://contacts/list` | `username` | `{addressbooks:[{…, objects}], count, home}` |
| `txco://contacts/list` | `username`, `addressbook`, `after` (modseq cursor), `limit` (≤ 1000) | `{items:[{name, path, uid, etag, fn, kind, addresses[], modseq, …}], count, next, sync_token}` — facts only, never the bytes |
| `txco://contacts/delete` | `username`, `addressbook`, `uid` or `name` | `{deleted, name, uid}` |

## What a client gets

Discovery through `/.well-known/carddav` → `<prefix>/` (current-user-
principal) → `<prefix>/<username>/` (addressbook-home-set) →
`<prefix>/<username>/addressbooks/` (the books) — so a client needs the
server, the username and the password. Then `PROPFIND`, `REPORT`
(`addressbook-multiget`, `addressbook-query`), `GET`, `PUT` (with
`If-Match` / `If-None-Match`; `201` on create, `204` on update),
`DELETE`, `MKCOL`, `PROPPATCH` (display name, description). Objects keep
stable URLs, UIDs and ETags across re-materialization.

A client's bytes are stored as written (line endings normalized) and come
back byte-for-byte on `GET` and in a multiget — so a photo, an `X-ABLabel`,
an escaped `\;` in a note survive. Apple's **groups** are ordinary cards
(`X-ADDRESSBOOKSERVER-KIND:group` with `X-ADDRESSBOOKSERVER-MEMBER` lines);
the parse reports them as `kind: group`. Not yet: `sync-collection` /
`getctag` (clients fall back to an etag diff per refresh, which works);
`addressbook-query` matches the first value of a property only and
case-sensitively (the library's matcher), and its results are re-encoded
by the library, which emits a `;` inside a text value unescaped.

Basic auth on every request: the head verifies argon2id once and caches
the verified triple for five minutes. A request must arrive over TLS — the
web head's own listener, or `X-Forwarded-Proto: https` from the front
proxy — unless `--contacts-insecure-auth` (dev). Throttles count cache
misses only (`--contacts-login-rate`). A `disabled` account and a
suspended tenant are refused; an account on another tenant's hostname is
refused exactly like a wrong password.

## Seeding address books with a stack

A stack may ship whole address books declaratively in the reserved
`CONTACTS/` tree, a pack kind beside `VECTORS/`, `KV/`, `BLOBS/` and
`CALENDARS/` (deployed by `txco data apply`, mirrored live by `txco dev`):

```
OPS/<stack>/
  CONTACTS/
    paris@pony.example.com/team.jsonl     → the address book "team" of that account
```

```jsonc
{"addressbook":{"display_name":"Paris team","description":"seeded"}}
{"name":"bob.vcf","card":{"fn":"Bob Example","emails":[{"value":"bob@example.com"}]}}
{"name":"raw.vcf","vcard":"BEGIN:VCARD\r\nVERSION:3.0\r\n…"}
```

The optional `addressbook` line sets the display fields (and `policy`);
every other line is one card — `card{}` as `txco://contacts/put` takes it
or `vcard` text. On activation the book is ensured, every card put, and
every live card the pack no longer lists is deleted — the pack is the
book's desired state. The **account must already exist**; a pack for an
unknown account is an error for that pack, logged, activation unaffected.
A book the pack creates denies client `put`/`delete` unless its header
says otherwise: keep runtime-written books out of packs.

## Policy: what a stack hears, and when

Per address book, five verbs — `put`, `delete`, `mkaddressbook` (a client
creates a book), `remove` (a client deletes one), `proppatch` — each one
of `deny` (403, the default for `mkaddressbook` and `remove`), `local`
(the default for `proppatch`), `observe` (commit, then tell the
`_contacts` stack; the default for `put` and `delete`) or `stack` (ask the
`_contacts` stack **first**; 403 unless it answers `@contacts.res.ok =
true`). Resolution: the book's `policy`, then the account's, then the
chassis default. The `_contacts` stack is the subscription — without it
`observe` is silent and `stack` answers `503`.

### The envelope

```
@src                  "contacts"                          @client.ip
@contacts.tenant      (slug)         @contacts.account    (username)
@contacts.phase       observe | answer                    @contacts.op   put | delete | mkaddressbook | remove | proppatch
@contacts.addressbook {id, name, display_name}
@contacts.object      {name, uid, etag, prior_etag, size, exists}     (put / delete)
@contacts.vcard       the client's card, as written                   (put)
@contacts.card        {…the parse of it: uid, fn, emails[], addresses[], kind, …}
@contacts.prior       {card: {…the stored card's parse…}}             (put on an existing object, delete)
@contacts.props       {displayname, description}                      (mkaddressbook / proppatch)
```

A rule may `EMIT @delete = &array("@contacts.vcard", "@contacts.card",
"@contacts.prior")` once it has consumed them.

### Answering (`@contacts.phase == "answer"`)

```txcl
WHEN @contacts.phase == "answer" && @contacts.op == "put" && @contacts.card.addresses.0 !~ /./
  EMIT @contacts.res.ok = false, @contacts.res.code = "cannot",
       @contacts.res.msg = "add an email address to this card"

WHEN @contacts.phase == "answer"
  EMIT @contacts.res.ok = true
```

`ok` absent or false is a `403`; `code` is `cannot` (403), `limit` (507)
or `unavailable` (503); `msg` is shown to the client. On `ok`, an optional
`@contacts.res.card` (or `.vcard`) is the card the head **commits instead
of the client's bytes** — the client's UID is kept. Most stacks accept a
card as written: the client's own rich card is what its owner wants to
keep. The head waits `--contacts-resp-timeout` (30 s).

## Client settings

The head never routes by name: it serves every hostname the web head
does, so **the server a contacts app should use is the domain of the
address** — `paris@<stack>.stacks.example` connects to
`https://<stack>.stacks.example/carddav/`, and `/.well-known/carddav` on
that host does the rest. A user name typed without its domain (`paris`)
completes to the server's host, so an account dialog that shows only the
local part still logs in. For clients that discover a server from an
address by DNS (RFC 6764), the `dns` personality can publish
`_carddavs._tcp` SRV + TXT records (`--dns-carddavs-port`, see
[dns](./dns.md)).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--contacts-path-prefix` | `/carddav` | The reserved prefix on every hostname (plus `/.well-known/carddav`); must differ from the calendar's |
| `--contacts-store` / `--contacts-db-path` | `sqlite` / `./chassis/data/contacts.db` | The index; a non-sqlite backend is shared and opened on every node |
| `--contacts-insecure-auth` | `false` | Accept Basic auth without TLS (`txco dev --contacts` sets it) |
| `--contacts-login-rate` | `30` | Verifications per minute, per IP and per username, on cache misses only |
| `--contacts-object-max-bytes` | 1 MiB | Size cap for a card (ops and client PUT; advertised as `max-resource-size`) |
| `--contacts-resp-timeout` | `30s` | Answer-lane deadline |
| `--contacts-observe-sample` / `--contacts-observe-max-inflight` | `1` / `8` | Observe-lane sampling and concurrency |

Env: `TXCO_CONTACTS_*`. Example: `examples/contacts-hello`.
