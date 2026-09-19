# Drive

A stack can keep a **mutable document space** — a folder tree a person
mounts in Finder, syncs with rclone, or that a pony's documents live in —
under a collection the `webdav` personality serves at
`https://<host>/drive/` and the `txco://drive/*` ops write and read. `FILES/`
is a stack's `/usr/share`: deployed with the code, public. Blobs are its
`/var/lib`: named content, permissioned, over the immutable content store.
The drive is its `/home`: files with paths that clients rename, rewrite and
delete, where the thing that stays fixed is the **resource id**.

> A path is where a resource is. A resource id is which resource it is.
> An etag is which version of it you have.

Three storage planes, then, and the drive is the only mutable one. Bytes
live in an ordinary object store (a directory in open core; S3 in the
hosted build) under keys the store mints, one per version, never
rewritten. Names, hierarchy, sizes, etags and the change sequence live in a
SQL index (a SQLite file in open core; Postgres in the hosted build). A put
streams the body to a fresh object first and writes the row second, so an
object without a row is an orphan the sweeper reclaims — never a row
without bytes.

## Identity

| fact | field | changes when |
|---|---|---|
| which resource | `resource_id` (`dr_…`, minted once) | never — a rename keeps it; delete + recreate mints a new one |
| where it is | `path` (below the collection root, NFC, `/`-joined) | move |
| which version | `etag` (sha256 of the current bytes; `""` for a directory) | every content change |
| when, in order | `modseq` (the collection's `sync_token` at the row's last change) | every change to the row |

A consumer that keys on the resource id survives renames; one that keys on
the etag re-processes every content change; one that remembers a
`sync_token` and asks `drive/list since = <token> include_deleted = true`
learns every change since, deletions included, in one page.

Deleted resources leave a **tombstone** (their row with `deleted_at` set
and a bumped `modseq`) for `--drive-tombstone-retention` (7 days) so a
`since` listing can report the deletion; the path is free at once, and the
bytes are reclaimed after `--drive-sweep-grace` regardless.

## Collections and accounts

A **collection** is a named folder tree of the tenant (`name` is a URL
segment; a pony's collection is its slug). An **account** is a Basic-auth
login for the `webdav` head — `<local>@<domain>` where the domain is a
verified hostname or delegated zone of the tenant, the same rule the
calendar and contacts heads apply, so usernames are globally unique by
construction — bound to exactly ONE collection, which is the root the
login sees. The account holds no password: its username signs in as its
`principal`, with a [credential](./users.md#credentials) whose scopes name
the drive (`drive:<collection_id>:*`). The same credential can open the
pony's mailbox, calendar and contacts too, if its scopes name them.

```txcl
EXEC "txco://drive/collection" WITH name = ._pc.slug, into = "_dc"
EXEC "txco://drive/account"
  WITH username   = ._imapacct.username,
       principal  = &concat("pony:", ._pc.slug),
       collection = ._pc.slug,
       into       = "_drvacct"
# → _drvacct = {username, created, principal, collection_id, collection, mount: "/drive/paris/"}
```

## Operations

All eleven are `txco://drive/*`, tenant-scoped, and answer at `into` (default
`_drive`, private to the flow). Errors are `<into>.error.{code, message}`
with the op's own result absent — handle with `WHEN ._drive.error`. A
collection is always addressed by `collection` (its name); a resource by
`path` or, where noted, `resource_id`.

| op | WITH | result |
|---|---|---|
| `drive/collection` | `name`; `remove` (+ `force` to remove a non-empty one); `policy` (what a CLIENT may do, by subtree — see below) | `{id, name, sync_token, bytes_used, resource_count, created, policy?}` or `{name, removed}` |
| `drive/account` | `username`, `collection` (on create), `principal` (on create); `status` `active` \| `disabled` | `{username, created, principal, collection_id, collection, mount}` |
| `drive/put` | `collection`, `path`; `from` XOR `value` XOR `from_sha` (a sha256 the tenant already holds in the blob store — streamed, not buffered, so the op cap does not apply); `encoding` `base64` (default) \| `utf8`; `content_type`; `if_match`, `if_none_match` (an etag, an RFC 7232 list of them, or `*`); `parents` (create missing ancestors) | `{resource_id, path, etag, size, created, noop, modseq}` — `noop` when the bytes were already there |
| `drive/get` | `collection`; `path` XOR `resource_id`; `encoding` `base64` (default) \| `utf8` \| `auto`; `max_bytes` | `{resource_id, path, name, etag, size, content_type, modseq, content, encoding}` |
| `drive/stat` | `collection`; `path` XOR `resource_id` (`path = ""` is the root) | `{exists, resource?}` — a miss is a result |
| `drive/list` | `collection`; `path` (a directory, `""` = root); `recursive`; `since` (a sync token; implies recursive); `include_deleted`; `limit` (≤ 1000, default 200); `after` | `{items[], count, next, sync_token}` |
| `drive/delete` | `collection`; `path` XOR `resource_id`; `if_match` (as `drive/put`) | `{deleted, resource_id?, path?, kind?, modseq?}` — a miss is `{deleted: false}`; a directory takes everything below it |
| `drive/mkdir` | `collection`, `path`; `parents` | `{resource_id, path, created, modseq}` — an existing directory is `created: false` |
| `drive/move` | `collection`, `path`, `to`; `overwrite`; `parents` (for `to`) | `{resource_id, path, from, kind, etag, modseq}` — the id is kept |
| `drive/copy` | `collection`, `path`, `to`; `overwrite`; `parents` | the same shape; a NEW id (every descendant too) |
| `drive/sign` | `collection`; `path` XOR `resource_id`; `ttl` (seconds, default 600, at most 3600) | `{url, expires_at, ttl, resource_id, path, name, size, content_type, sha256}` — a short-lived URL for that exact document; see [signed URLs](#signed-urls-a-file-by-reference) |

Every item of `list`, and `stat`'s `resource`, is
`{resource_id, kind (file | dir), path, name, parent, size, content_type,
etag, modseq, created_at, updated_at, deleted?, deleted_at?}`.

Bytes through the ops are buffered JSON, capped by `--drive-op-max-bytes`
(32 MiB): a larger file is stored and served by the head but an op reads it
only up to the cap (`txco_drive_too_large`, or lower with `max_bytes`).
`put` and `get` pay `FuelCostDrivePerMiB` per MiB moved on top of the
dispatch fuel (`sign` pays it for the read it authorizes); the head is not
fuel-metered.

Error codes: `txco_drive_{no_tenant, disabled, invalid_arg, not_found,
exists, no_parent, is_directory, not_directory, precondition, quota,
too_large, cycle, not_empty, username_taken, domain_not_owned,
sign_unavailable, store}`.

## Signed URLs: a file by reference

`drive/get` puts a file's bytes IN the run, base64, under the op cap. That
is the wrong shape for a document on its way to something outside the
chassis — a parser, a transcoder, a model that takes a URL: the bytes ride
every envelope between the read and the call, and a large file cannot make
the trip at all. `drive/sign` hands out a reference instead:

```txcl
EXEC "txco://drive/sign"
  WITH collection = ._ing.slug, resource_id = ._ing.resource_id, into = "_sig"
# → _sig = {url: "https://…/_txc/signed/v1.…", expires_at, ttl: 600,
#           resource_id, path, name, size, content_type, sha256}

EXEC "op://parse_doc"
  WITH content_url = ._sig.url, content_sha256 = ._sig.sha256, …
```

The URL answers `GET` and `HEAD` (ranges included) on the web head, with no
other credential: **whoever holds it can read that document until it
expires.** What keeps that narrow:

- **One document, exactly.** The URL names the file's content (`sha256`) as
  well as its address. If the file is rewritten the URL answers `410 Gone`
  — it never reads bytes that were not there when it was signed. A rename
  keeps the `resource_id`, so the URL survives it; a delete ends it.
- **Short-lived.** Ten minutes by default, an hour at most. The fetch must
  *start* inside the window; a download already streaming is not cut off.
- **Nothing to guess.** Malformed, forged, expired, deleted, and "that
  tenant is suspended" are the same bare `404`.
- **Inert.** The path answers on every ordinary hostname the web head
  serves, so the response is always an attachment — `nosniff`, sandboxed,
  `no-store` — and can never render as a page on someone's origin.
- **Stateless.** The token is the claims and an HMAC under a key derived
  from the secret store's master key; nothing is stored, and any node of a
  fleet sharing that key serves any node's URLs. No master key on the node ⇒
  `txco_drive_sign_unavailable`. Rotating the master key ends every live URL.

The fetcher should verify what it received against `sha256`: the URL
guarantees it, but the fetcher is the one who can prove it.

Treat the URL as a secret for its lifetime. The chassis keeps the token out
of its access log (`/_txc/signed/<redacted>`), but the op's result is part of
the run like any other value — it appears in the trace — so keep `ttl` short
and `EMIT @delete` the URL once the call that needed it has been made.

`--signed-url-base` is the public origin the URLs are minted on
(`https://files.example.com`): any name that reaches the web head works,
because the token, not the hostname, decides what is read. Empty derives
from `--continuation-callback-base-url`, then `--fqdn` and the web
listener, which is right for a single node and rarely right behind a proxy.

## Reserving a subtree for the stack

A drive can have two writers with different jobs: the person at the mount,
and the stack that curates what the person dropped. A collection's `policy`
says which verbs a **WebDAV client** may use on which subtree, so a folder
the stack owns is not rewritten by a client that thinks it knows better.

```txcl
EXEC "txco://drive/collection"
  WITH name   = "paris",
       policy = &object("Knowledge", &object("write", "deny"))
```

That reads as: a client may list, read, rename, delete and make folders
under `Knowledge/`, but may not write a file's **content** there. Verbs are
`write` (PUT a file), `create` (MKCOL), `delete`, `move_in` and `move_out`
(the two halves of a MOVE; a COPY is judged at its destination). Modes are
`allow` and `deny`; an unnamed prefix, verb or mode allows, so a policy only
names what it refuses. The longest matching prefix decides, so a refused
tree can readmit one folder inside it. Passing `policy` again replaces the
whole thing and `&object()` clears it; leaving it out never disturbs one.

**The policy binds the head, never these ops.** `txco://drive/*` talks to
the store directly and is never checked, which is exactly what lets a stack
keep filing into a tree its clients may only read. It is therefore not a
boundary between principals — one account is bound to one collection, so
there is only ever one client — but a division of labour. A typo in a verb
or mode is refused at the write door rather than ignored, because a policy
that fails open would allow precisely what its author meant to refuse.

Client bookkeeping is exempt from `write`: a dot-file (`.DS_Store`, and the
`._name` AppleDouble sidecars macOS writes into every folder it merely
displays) is always allowed, or browsing a reserved folder in Finder would
be a stream of error dialogs.

## Mutation events

Every committed mutation is reported to the tenant's `_scheduled` stack as
one scheduled event, due now, when the node has a scheduled store: the
poller fires `_scheduled/0` with `@src == "scheduled"` and the facts under
`@scheduled.payload`:

```json
{"event": "drive.resource.created",
 "drive": {"tenant": "tnt_…", "collection_id": "dc_…", "collection": "paris",
           "resource_id": "dr_…", "kind": "file", "path": "data/brief.md",
           "etag": "…", "size": 1834, "content_type": "text/markdown",
           "modseq": 42, "at": "2026-09-16T12:00:00Z"}}
```

`event` is one of `drive.resource.{created, updated, deleted, moved}`
(`moved` also carries `from_path`). A directory move, copy or delete
reports the directory's own event first and then one event per FILE below
it, all at the same `modseq` (`moved` per file with its own `from_path`;
`created` per copied file with its new id; `deleted` per file); an
overwriting move or copy reports the replaced destination's `deleted`
events before its own. Subdirectories get no event of their own. So a
consumer that indexes files hears about every file a folder rename or
delete touched without walking the tree. The event carries no bytes: a
consumer that wants them calls `drive/get` by `resource_id`. The
idempotency key is `drive:<resource_id>:<modseq>`, so two quick writes to
one resource are two events.

With `--drive-store` and `--scheduled-store` on the SAME shared database
(the hosted build) the event row commits with the mutation; otherwise it
is enqueued after commit and boot says so.

## Configuration

| flag | default | |
|---|---|---|
| `--drive-store` | `sqlite` | index backend; `postgres` in the hosted build |
| `--drive-db-path` | `./chassis/data/drive.db` | the bundled index file |
| `--drive-objects` | `file` | object backend; `s3` in the hosted build |
| `--drive-objects-file-dir` | `./chassis/data/drive` | the bundled object root |
| `--drive-max-file-bytes` | 4 GiB | one file |
| `--drive-max-collection-bytes` / `--drive-max-resources` | 0 (unlimited) | per collection |
| `--drive-op-max-bytes` | 32 MiB | `put` / `get` through the ops |
| `--signed-url-base` | derived | public origin `drive/sign` mints URLs on |
| `--drive-sweep-period` / `--drive-sweep-grace` / `--drive-tombstone-retention` | 900 s / 1 h / 7 d | the sweeper, on `webdav` nodes |

An object backend implements `Put`, `Get`, `Stat`, `List`, `Delete`. It may
also implement `GetRange` (`drive.RangeReader`) to open part of an object;
the S3 backend does. The store returns every object as a seekable reader
sized from the index, so a client's `Range` is positioned before anything
is opened: a backend with `GetRange` serves only that slice, one whose
`Get` returns a seeker (the file backend) is seeked, and any other is read
from the start with the prefix discarded — correct on every backend, cheap
on the ones that matter.

The index opens on a node when `webdav` is in `--personalities` (fatal if
it fails) or when `--drive-store` names a shared backend (warn-and-continue;
the ops answer `txco_drive_disabled` until restart). The head itself is
documented in [protocols/webdav](./protocols/webdav.md).
