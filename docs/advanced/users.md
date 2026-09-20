<!-- nav: Users & credentials -->

# Users and credentials — the people and things your product knows

_`txco://user/*` and `txco://credential/*` are where a stack keeps **who**:
the people who use your product, the other things that act in it (a pony, a
service), and the revocable passwords each of them signs in with. One
person is one user across the tenant, with as many credentials as they have
devices — each scoped to the doors it may open, each revocable on its own._

> **Upgrading (printing).** The print head signs in with these credentials
> too. Its `IPP_PASSWORD` secret is gone: register each printer with
> `txco://ipp/printer` and issue its principal a credential with an `ipp`
> scope ([IPP](./protocols/ipp.md#turn-it-on)).
>
> **Upgrading.** Accounts no longer hold passwords. `txco://imap/account`,
> `calendar/account`, `contacts/account` and `drive/account` take a
> `principal` instead and refuse `password` and `rotate`; every mail,
> calendar, contacts and drive client signs in with a credential issued
> here. A password set before this release has no credential id and stops
> working: re-run each account op with its `principal`, issue a credential,
> and give the new password to the client ([Signing in](#signing-in)).

These are **your product's** users. They are not the accounts that
administer a tenant (`txco login`, the admin UI): those live on a different
plane, and nothing links the two.

## Principals

A **principal** is whoever a credential signs in _as_ — the thing a later
access decision is asked about. It is written `<kind>:<name>`:

| principal | what it is |
|---|---|
| `user:usr_7HqZ3kYb…` | a person. The chassis mints it (`txco://user/create`); it has a user record with a name and a status. |
| `pony:paris`, `service:billing-sync`, … | anything else your product names. No user record — it exists as soon as it has a credential or a binding. |

`kind` is lowercase letters, digits, `_` and `-`. `name` is letters, digits
and `. _ @ + -`. Ids are case-sensitive. You cannot name a `user:` principal
into existence; only `user/create` makes one.

A principal is **found** by an identifier bound to it. `user/create` binds
the user's email; a login's username resolves the same way.

## The ops

All six are tenant-scoped, and answer at `into` (default `_user` or
`_credential`).

```txcl
# a claim flow: the magic link was clicked, so the address is proven
WHEN ._claim.ok == true
  EXEC "txco://user/create"
    WITH email        = ._claim.email,
         display_name = ._claim.name,
         verified     = true
# → _user = {id, principal, display_name, status, email, email_verified,
#            created_by, created_at, updated_at, created}
```

| op | WITH | result |
|---|---|---|
| `user/create` | `email`, `display_name?`, `verified?` | the user, its `principal`, and `created` |
| `user/get` | `id` \| `email` | the user (`email`, `email_verified` when looked up by email) |
| `user/disable` | `id`, `disabled?` (default `true`; `false` re-enables) | the user |
| `credential/create` | `principal`, `scopes` (a list, or one scope as a string — build a computed one with `&concat`), `label?`, `password_style?` (`token` \| `words`), `password_words?` (4–12) | the credential **and its `password`, once** |
| `credential/list` | `principal`, `include_revoked?` | `{principal, count, items[]}`, newest first — never a secret |
| `credential/revoke` | `id` (with `principal` to pin whose it must be) — or `principal` with `except` = an id, or `all = true` | `{id, principal, revoked}` — or `{principal, revoked_count}` |

**`user/create` is safe to retry.** It is idempotent on the email: creating
a user whose address your stack already holds returns that user with
`created: false`, so a redelivered message or a resumed task does not fail
and does not split one person in two. `verified = true` upgrades an
unverified address; nothing downgrades it. The display name is set on
create only.

**Disabling revokes nothing.** A disabled user cannot be issued new
credentials and will not be able to sign in, but their credentials are
untouched, so `disabled = false` restores exactly what they had.

## Credentials

```txcl
# only for a user this run created — a retried claim finds the user again
# (`created: false`) and must not mint a second password
WHEN ._user.created == true
  EXEC "txco://credential/create"
    WITH principal      = ._user.principal,
         scopes         = ["imap:*:*", "calendar:*:*"],
         label          = "Alice's laptop",
         password_style = "words",
         redact         = "_credential.password"
# → _credential = {id, short_id, principal, kind, scopes, label,
#                  created_by, created_at, password}
```

The `password` is in the result **once**. Only its argon2id hash is stored;
it cannot be read back, by you or by the chassis. Show it to the person and
drop it. The chassis keeps it out of [traces](./trace.md) on its own; a
`redact` as above is harmless.

**Every password is generated.** There is no `password = "…"` param. An
issued password carries the id of its credential:

```text
words   k7m2-river-galaxy-bamboo-orbit-velvet     for a person to type
token   txc_k7m2_p3vw8n…                          for a machine
```

That leading `k7m2` is the credential's `short_id`. A principal can hold
many credentials, and the id is how a login finds the right one in a single
lookup instead of trying each. It is not a secret — it is also how someone
recognises "the one that starts k7m2" in a list when they want to revoke
it.

### Scopes

A credential opens only the doors its `scopes` name. Each is
`domain:instance:action`, where `instance` and `action` may be `*`:

| scope | opens |
|---|---|
| `imap:*:*` | mail, any mailbox the principal has |
| `drive:dc_7HqZ3k…:*` | one drive collection |
| `ipp:front-desk:print` | printing, to one printer |

Scopes only narrow. What a credential can do is what its principal may do
**and** what its scopes cover, so the password typed into the office
printer cannot read mail.

The domain is never `*`: a credential names the heads it opens. A head that
ships later is closed to every password already issued, until you issue one
for it. At least one scope is required, at most 32. A principal may hold 64
live credentials.

### Rotating

Issue the new credential, then revoke the rest:

```txcl
# 3400: issue
WHEN ._rotate.principal != ""
  EXEC "txco://credential/create"
    WITH principal = ._rotate.principal, scopes = ._rotate.scopes,
         into = "_new", redact = "_new.password"

# 3410: revoke every other credential
WHEN ._new.id != ""
  EXEC "txco://credential/revoke"
    WITH principal = ._rotate.principal, except = ._new.id
# → _credential = {principal, revoked_count}
```

`except` must name a live credential of that principal, and a bare
`principal` is refused. That is deliberate: if the issue step failed, its
id is missing — and the op must not answer a missing id by revoking the one
password that still works. To revoke everything, say so: `all = true`.

Revocation is permanent. The row stays (`include_revoked = true` lists it),
and its `short_id` is never reused.

### Revoking one device

```txcl
# "remove this device": the id came from the person's own request
WHEN ._req.ok == true
  EXEC "txco://credential/revoke"
    WITH id = ._req.credential_id, principal = ._req.principal
```

**Pass `principal` with an `id` that came from a request.** The
[creator-stack rule](#which-stack-may-manage-a-principal) is not a wall
between one stack's own principals: a product's stack manages every one of
its users, so a bare `id` would let any of them revoke another's
credential by guessing or learning its id. With `principal`, the id is
revoked only if it is that principal's; anyone else's answers `not_found`,
the same as an id that does not exist. Combining `id` with `except` or
`all` is refused.

## Signing in

The IMAP, CalDAV, CardDAV, WebDAV and IPP heads sign people in with these
credentials. An account op (`txco://imap/account` and its siblings) binds
the account's username to a principal; the client then presents the
username and a password `credential/create` issued to that principal:

```text
username ──binding──▶ principal ──id in the password──▶ credential ──▶ verified
                                                             │
                                          its scopes must open this head
```

| head | the scope a login needs |
|---|---|
| IMAP | `imap:<username>:login` |
| CalDAV | `calendar:<username>:login` |
| CardDAV | `contacts:<username>:login` |
| WebDAV | `drive:<collection-id>:login` — the account's own collection |
| IPP | `ipp:<printer>:print` — and the printer must be granted to the principal |

So `imap:*:*` opens every mailbox the principal has, and nothing else; one
credential with `["imap:*:*", "calendar:*:*", "contacts:*:*"]` is one
password for Mail, Calendar and Contacts. A drive account's collection is
what its principal may reach; the credential's `drive` scope can only
narrow it.

**Printing has no account op.** A printer is not a login: it is a row
([`txco://ipp/printer`](./protocols/ipp.md#the-printer-op)) that names the
one principal who may print to it. That principal signs in with a username
it already has — a user's email, or one an account op bound — so a pony with
a mailbox prints with its mail address. `ipp:*:*` opens every printer the
principal is granted, and nobody else's: the scope narrows, the grant
decides.

**Revocation is immediate.** Each head remembers a verified password for a
few minutes so a client's stream of requests costs one hash, but every
request reads the credential, so a revoked one is refused on its next use.
An IMAP session is the one thing that signs in once and stays: it re-checks
its credential about once a minute and is closed (`BYE`) within that minute
of a revocation ([IMAP](./protocols/imap.md#mail-client-settings)). A
disabled user cannot sign in until re-enabled, and loses open sessions the
same way.

**One budget for guesses.** Password checks are limited per client IP and
per principal across all five heads together (`--login-rate`, 30 a
minute): guessing over CalDAV spends the same budget as guessing over IMAP.
Behind a reverse proxy, name it in `--web-trusted-proxies` (and
`--imap-proxy-protocol`), or "per client IP" means the proxy and every
client shares one budget ([serve.md](./serve.md#behind-a-reverse-proxy-whose-address-is-it)).

**Who is acting.** A run a head starts on behalf of a signed-in client —
an IMAP answer lane, a CalDAV observe, a print job — carries `@principal.id`,
`@principal.kind` and `@principal.credential`. It is a read-only copy of
what the chassis pinned; no request, rule or op can set it, and a run
nobody signed in to has none. Usage lines and traces record the principal
too.

**The password stays out of traces.** The chassis removes a password
`credential/create` issued from every trace record of that request — as
itself, and inside a response body that renders it — without a `redact`.

## Which stack may manage a principal

**Only the stack that created a principal may change it.** Every record
carries the stack that wrote it (`created_by`). The first stack to write
anything for a principal — create the user, issue its first credential —
owns it, and from then on another stack's `user/disable`,
`credential/create`, `credential/list` or `credential/revoke` for it
answers `not_owner`, naming the owner. Granting it a printer
(`txco://ipp/printer`) follows the same rule, and needs the principal to
exist already.

- A **canary slot counts as its stack**: `web/canary` manages what `web`
  created, and so does a channel such as `web/_mail`.
- **`user/get` is open** to every stack in the tenant, and its `created_by`
  says whom to ask.
- To act on another stack's principal, jump into that stack
  ([`@goto`](./txcl/txcl.md#control-flow-via-_txc) = `"web/3400"`) and let
  its rules make the call.
- The stack is taken from the deployed rule that is running, never from
  the document, so it cannot be claimed by writing a field.

This keeps a stack that has no business with users from resetting a
password by mistake. It is not a wall between authors: whoever can deploy
one stack in a tenant can deploy them all.

## Errors

Failures land as `<into>.error.{code, message}` and the run continues:

| code (`txco_user_…` / `txco_credential_…`) | meaning |
|---|---|
| `invalid_arg` | a bad or missing param; the message says which |
| `not_found` | no such user, credential or `except` id **in this tenant** |
| `not_owner` | another stack manages this principal; the message names it |
| `identifier_bound` | the email already belongs to a different principal |
| `user_disabled` | a credential was requested for a disabled user |
| `too_many` | the principal already holds 64 live credentials |
| `no_tenant` · `no_stack` | the request has no tenant, or the op was not dispatched by a rule |
| `disabled` | this node could not open the identity database at boot |
| `store` | the database failed; the message has the detail |

## Where it lives

In the auth database (`--db-auth-dsn`): a local SQLite file by default, the
shared Postgres on a fleet, where every node sees the same users. Every
node opens it, not only those running the `admin` personality. A node that
cannot open it keeps serving everything else, and these ops answer
`disabled` there until it restarts.
