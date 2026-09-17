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
  WITH username = "paris@pony.example.com",
       collection = "paris",
       password_style = "words",
       into = "_drvacct"
# → _drvacct.password (once), _drvacct.mount = "/drive/"
```

The username is `<local>@<domain>` where the domain is a verified hostname
or delegated zone of the tenant — the rule every DAV head applies, so
usernames are globally unique by construction. Give the IMAP account's
password and one credential opens mail, calendar, contacts and files. In
a mount dialog a person may type just the local part; the head completes
it with the request's host.

## Mount

| client | how |
|---|---|
| macOS Finder | Go → Connect to Server → `https://pony.example.com/drive/`, user `paris` |
| Windows Explorer | Map network drive → `https://pony.example.com/drive/` |
| rclone | `rclone config` type `webdav`, url `https://pony.example.com/drive/`, vendor `other`; then `rclone sync ./docs pony:` |
| curl | `curl -u paris@pony.example.com -T brief.md https://pony.example.com/drive/brief.md` |

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
(Finder, rclone and most sync tools do) never overwrites a version it has
not seen. This is what a lock can honestly be on a fleet with no shared
lock state; it also keeps the head stateless across nodes. A LOCK on a
path that does not exist creates an empty file, as the RFC requires and as
Finder expects before its first PUT.

## Limits and housekeeping

| | |
|---|---|
| `--drive-max-file-bytes` (4 GiB) | a PUT that declares more is 413 before a byte moves; a PUT without a `Content-Length` (Finder streams a dragged file that way) is accepted and stops at the cap |
| `--drive-max-collection-bytes`, `--drive-max-resources` (unlimited) | a write past them is 507 |
| `--drive-login-rate` (30/min) | per client IP and per username, counted only on verified-login-cache misses; over it is 429 |
| `--drive-sweep-period` (15 min) | on `webdav` nodes: superseded versions and the objects of failed writes are reclaimed after `--drive-sweep-grace` (1 h); tombstones are hard-deleted after `--drive-tombstone-retention` (7 d) |

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

What a client does maps onto events like this: Finder creates a file as
LOCK (→ `created`, size 0), a zero-byte PUT (no event), then the bytes (→
`updated`); `cp` and rclone send the bytes at once (→ `created`). Finder
also writes `.DS_Store` and `._*` sidecars into every folder it touches; a
consumer that indexes files should skip any segment starting with a dot.

Ops reference: [drive](../drive.md).
