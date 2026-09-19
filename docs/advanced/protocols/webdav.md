<!-- nav: WebDAV -->

# WebDAV — a folder as the UI

_The `webdav` personality serves a tenant's [drive](../drive.md) to any
WebDAV client — macOS Finder, Windows Explorer, rclone, Cyberduck, curl.
Stacks write files into it with an op; a person drags files into it from
their desktop. The head is a generic file server: it knows no product and
never runs a stack to answer a request._

A drive is a mutable document space: files with paths that clients
rename, rewrite and delete. What stays fixed is the **resource id** —
minted once, kept through every rename and rewrite — so a stack that
indexed a document under its id is not confused when a person moves it.
Every committed mutation reaches the tenant's `_scheduled` stack after the
fact (`drive.resource.{created, updated, deleted, moved}`); a consumer
fetches the bytes it wants by id with `txco://drive/get`.

The [calendar](./calendar.md) and [contacts](./contacts.md) personalities
are this one's siblings: same account model, same Basic-auth flow, same
place on the web head. What differs is that WebDAV has no discovery and no
principal — a login is bound to ONE collection and that collection is the
root the client sees, so the mount URL is just the prefix.

## Turn it on

```
txco serve --personalities cron,web,admin,webdav
```

There is no listener of its own: the head mounts on the **web head** under
a reserved path prefix on every hostname it serves — `--drive-path-prefix`
(default `/drive`). It must differ from the calendar and contacts prefixes
(the chassis refuses to start otherwise). TLS is the web head's. The index
lives in its own SQLite file (`--drive-db-path`) and the bytes in a
directory (`--drive-objects-file-dir`); the hosted build points both at
shared backends (`--drive-store=postgres`, `--drive-objects=s3`) so every
node serves one drive. For `txco dev` on plain HTTP add
`--drive-insecure-auth`.

## Provision

```txcl
EXEC "txco://drive/collection" WITH name = "paris", into = "_dc"
EXEC "txco://drive/account"
  WITH username   = "paris@pony.example.com",
       collection = "paris",
       principal  = "pony:paris",
       into       = "_drvacct"
# → _drvacct.collection_id = "dc_…", _drvacct.mount = "/drive/paris/"

WHEN ._drvacct.created == true
  EXEC "txco://credential/create"
    WITH principal = ._drvacct.principal,
         scopes    = &concat("drive:", ._drvacct.collection_id, ":*"),
         password_style = "words"
# → _credential.password (once)
```

The username is `<local>@<domain>` where the domain is a verified hostname
or delegated zone of the tenant — the rule every DAV head applies, so
usernames are globally unique by construction. The account holds no
password: it signs in as its `principal` with a
[credential](../users.md#credentials) whose scopes name the drive. A login
opens `drive:<collection-id>:login` — the account's own collection is what
the principal may reach, and the credential's scope can only narrow it:
`drive:dc_…:*` opens that one drive, `drive:*:*` whichever the account is
bound to, and a credential without a `drive` scope opens none. In a mount
dialog a person may type just the local part; the head completes it with
the request's host.

## Mount

| client | how |
|---|---|
| macOS Finder | Go → Connect to Server → `https://pony.example.com/drive/paris/`, user `paris` |
| Windows Explorer | Map network drive → `https://pony.example.com/drive/paris/` |
| rclone | `rclone config` type `webdav`, url `https://pony.example.com/drive/paris/`, vendor `other`; then `rclone sync ./docs pony:` |
| curl | `curl -u paris@pony.example.com -T brief.md https://pony.example.com/drive/paris/brief.md` |

**Name the collection in the URL.** A client takes the volume's name from
the last path segment, so the bare `/drive/` mounts as "drive" on every
account — mount two and you get "drive" and "drive 1". Ending the URL with
the collection's own name mounts it as "paris". Both forms address the same
tree and a session gets its hrefs back in the form it used, so anything
already mounted at `/drive/` keeps working. `drive/account` answers with
the named form in `mount`. The one cost of the alias: a top-level directory
that shares the collection's name is addressed one level in, at
`/drive/paris/paris`.

The head answers OPTIONS with `DAV: 1, 2, 3` and LOCK/UNLOCK, which is what
Finder needs to mount read-write; PROPFIND `Depth: infinity` is refused
(403), as the RFC permits, so a listing is always one directory.

### A folder the stack owns

A collection can reserve a subtree from clients: `txco://drive/collection`
takes a `policy` of path prefix → verb → `deny`, the head enforces it, and
the stack's own ops are never subject to it ([drive](../drive.md)). The case
it exists for is a curated folder — the stack puts documents somewhere and a
desktop client must not overwrite them. It is worth reaching for, because a
client with a stale cache can re-upload a file nobody edited: macOS did
exactly that in the field, replacing a good PDF with one spliced at a 16 KiB
boundary, and the server stored it faithfully because a WebDAV PUT carries
no end-to-end checksum. Denying `write` on that tree is what makes the
question moot.

### Locks are a courtesy, not a guarantee

LOCK mints a fresh token and UNLOCK answers 204; nothing is stored and
nothing is excluded. Two clients editing one file both "hold" a lock. The
real protection is the etag: every PUT and DELETE honours `If-Match` /
`If-None-Match` against the current sha256, and a client that sends them
never overwrites a version it has not seen. This is what a lock can
honestly be on a fleet with no shared lock state; it also keeps the head
stateless across nodes — a LOCK, a PUT and an UNLOCK may each land on a
different machine, and none of them consults lock state, so none of them
can disagree.

**A LOCK on a path that does not exist creates nothing.** It answers 200
with a token, as it would for a file that exists, and the file comes into
being when the client writes it. RFC 4918 says such a LOCK must create an
empty resource, and this head once did — which left ghosts. A stack that
files a document away moves it out from under a client that still has the
old name cached; macOS takes a write lock even to *preview* a file, so the
next glance at the stale name LOCKed it and the name came back as a
zero-byte file nothing would ever clean up. A lock that reserves nothing
should not write anything either. Clients are built for this: a lock on a
missing name was only ever a reservation on servers with RFC 2518's
lock-null resources (Apache), RFC 4918 appendix D tells clients to expect
either model, and macOS does not depend on it at all — it creates a file
with a zero-byte PUT *before* it locks. The stale-name case now ends the
way it should: LOCK 200, GET 404, and the client drops the entry. The lock
still refuses what the write would refuse — a missing parent directory is
409, a tree whose policy denies `write` is 403.

Because the etag is the guarantee, the conditional headers are parsed as
RFC 7232 writes them rather than as the one bare etag most clients send:
a list (`If-Match: "a", "b"`) is satisfied by any entry, a comma inside
the quotes belongs to the etag, and `If-Match` compares strongly, so a
`W/` entry never satisfies it while `If-None-Match` ignores the prefix.
Both are applied inside the write's own transaction, so nothing slips
between the check and the change.

What none of this can catch is a client that lies about its own bytes.
The etag is the sha256 of what the server received, and WebDAV carries no
end-to-end checksum on a PUT, so a client uploading from a damaged cache
gets an etag that faithfully certifies the damage. Guarding against that
is a matter of refusing the write (see the collection policy above), not
of validating it.

Note also that macOS takes a write lock even to open a file for reading.
A subtree whose policy denies `write` therefore refuses the LOCK as well
as the PUT: telling the client up front is what makes it open the file
read-only, instead of discovering the refusal at save time and retrying
until the mount stalls.

### Reads are ranged

A GET honours `Range` on every backend — `206` with `Content-Range`, a
suffix (`bytes=-64`), a range that runs past the end (clamped, never
padded), `416` with the file's length when it starts past the end, and
`If-Range` against the etag so a client resuming into a changed file gets
the whole new file rather than a splice. HEAD advertises `Accept-Ranges:
bytes`.

This is not a nicety. macOS webdavfs serves a read it has not cached as a
Range GET, and a PDF reader's first read is the tail (the cross-reference
table lives there). A server that answers a Range request with `200` and
the whole file is *accepted* by the client, which takes the file's head
as its tail — and every PDF opens as "damaged" while `shasum` over the
mount says the bytes are perfect (prod, 2026-09-17). The store returns
each object as a seekable reader whose size is the index's, so the range
is positioned before anything is opened, and a backend that can open part
of an object (S3) sends only that part through the server.

## Limits and housekeeping

| | |
|---|---|
| `--drive-max-file-bytes` (4 GiB) | a PUT that declares more is 413 before a byte moves; a PUT without a `Content-Length` (Finder streams a dragged file that way) is accepted and stops at the cap |
| `--drive-max-collection-bytes`, `--drive-max-resources` (unlimited) | a write past them is 507 |
| `--login-rate` (30/min) | password checks per client IP and per principal, shared with the IMAP, CalDAV and CardDAV heads, counted only on verified-login-cache misses; over it is 429 |
| `--drive-sweep-period` (15 min) | on `webdav` nodes: superseded versions and the objects of failed writes are reclaimed after `--drive-sweep-grace` (1 h); tombstones are hard-deleted after `--drive-tombstone-retention` (7 d) |

Every refused login logs one `webdav login` line with its outcome; a
successful one logs only when it checks the password (once per 5-minute
login cache per node), not on every request a mounted drive makes. The
`chassis.webdav.logins` metric still counts every outcome, cache hits included.

A PUT streams straight to the object store — memory is not the bound —
with a read deadline that scales with the declared size, so a 4 GiB upload
gets its two hours while an abandoned stream is still reaped.

## What a stack sees

Nothing, until it subscribes: the drive's mutations are `_scheduled`
events, so a stack with an `OPS/_scheduled/` tree keyed on
`@scheduled.payload.event =~ /^drive\./` hears every write. The payload
carries the facts (collection, resource id, path, etag, size, content
type) and never the bytes; `txco://drive/get` by `resource_id` fetches
them. A folder rename, copy or delete reports the folder and then every
file below it, one event each, so a consumer keyed on files never walks
the tree. A file the stack chooses not to parse is still there for the
client — **stored, not indexed** is a valid state, and the stack's own
status record is where it says so.

What a client does maps onto events like this: macOS creates a file as a
zero-byte PUT (→ `created`, size 0), a LOCK (no event — a lock never
writes), then the bytes (→ `updated`) and an UNLOCK; curl and rclone send
the bytes at once (→ `created`). Finder
also writes `.DS_Store` and `._*` sidecars into every folder it touches; a
consumer that indexes files should skip any segment starting with a dot.

Ops reference: [drive](../drive.md).
