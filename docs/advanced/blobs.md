# Blobs

A stack can keep **runtime-writable bytes** — an uploaded PDF, a generated
report, a user's document — under a mutable name, with permissions, over the
same content-addressed store that holds `FILES/` and `DATASETS/`. `FILES/`
is a stack's `/usr/share`: deployed with the code, public. Blobs are its
`/var/lib`: written by operations at runtime, private by default.

> Names identify mutable concepts. Hashes identify immutable facts.

Two planes: the **content store** holds immutable values by sha256 (every
version of every blob, deduplicated); the **name index** holds mutable
pointers — `faqs/house-01.doc` → a sha — plus the permissions the name
requires. Re-uploading a document repoints its name (a *replace*), while the
old bytes stay addressable by hash (*history*). An application's own
provenance records hang meaning off unnamed hashes.

## Operations

All five are `txco://blob/*`, tenant-scoped, and answer at `into` (default
`_blob`, private to the flow). Errors are a root-level `blob.error.{code,
message}` — handle with `WHEN @blob.error`.

```txcl
# retain an upload under a name derived from its filename
WHEN ._upload.ok == true
  EXEC "txco://blob/put"
    WITH under = "paris/docs",
         filename = ._upload.filename,
         from = "._upload.content_b64",
         permissions = &array("blob:guest:read"),
         grants = &array("blob:*:*")
# → _blob = {name, sha256, size, existed, replaced}
```

| op | WITH | result |
|---|---|---|
| `blob/put` | address (see below); `from` XOR `value`; `encoding` `base64` (default) \| `utf8`; `content_type`; `filename`; `permissions` | `{name?, sha256, size, existed, replaced?}` |
| `blob/get` | `name` XOR `sha256`; `encoding` `base64` (default) \| `utf8` \| `auto`; `max_bytes` | `{name?, sha256, size, content_type, filename?, content, encoding}` |
| `blob/stat` | `name` XOR `sha256` | `{exists, sha256?, size?, content_type?, filename?, permissions?, updated_at?}` — a miss is a result |
| `blob/list` | `prefix`, `after`, `limit` (≤ 200) | `{names: [{name, sha256, size, content_type, filename?, permissions, updated_at}], next, count}` |
| `blob/delete` | `name` | `{deleted}` — unlinks the name; the bytes stay |

`existed` on a put means *this tenant already held those bytes*; `replaced`
means the name pointed at different bytes before. `txco kv list _txc.blob`
shows an operator the tenant's names.

Codes: `txco_blob_no_tenant`, `txco_blob_invalid_arg`,
`txco_blob_invalid_name`, `txco_blob_not_found`, `txco_blob_denied`,
`txco_blob_too_large`, `txco_blob_disabled` (no content store on this
node), `txco_blob_store`.

## Addressing

Three ways to name what you put:

- **A literal name** — `name = "faqs/house-01.doc"`. `/`-separated segments
  of `[A-Za-z0-9._-]`, no `.`/`..`, no segment starting with `_`, at most
  250 bytes. Developers writing identifiers.
- **A derived name** — `under = "paris/docs", filename = <anything>` gives
  `paris/docs/<sha256 of the NFC-normalised filename>`. A total function of
  any real filename (spaces, unicode, emoji, 200-byte titles), so a
  drag-and-drop upload can never fail on its name, and the same title always
  derives the same name — which is what makes a re-upload a replace. The
  verbatim `filename` rides as metadata. The result returns the concrete
  `name`; keep it as your document key.
- **No name** — a content-only put returns `{sha256, size, existed}`. Right
  for machine values (projection outputs) whose meaning lives in your own
  records; wrong for user documents, which want replace, not append.

`get` and `stat` take `name`, or `sha256` for the privileged by-hash path.

## Permissions

A name may require permissions — Shiro-style `blob:<audience>:<action>`
strings, matched by the chassis capability matcher. Permissions attach to
the **name**, never the bytes: the same sha can sit behind `guest/menu.pdf`
(requires `blob:guest:read`) and `internal/menu.pdf` (requires
`blob:internal:read`), so declassification is always minting a new name.

The permission set an operation holds is its **declared context**:
`WITH audience = "guest"` means it holds `blob:guest:read`; `WITH grants =
&array("blob:cas:read", "blob:*:read")` lists explicit strings (wildcards
compose on the grant side only). Every access to a name — read, repoint,
delete, stat — needs the context to cover all of the name's permissions; a
name without permissions is open within the tenant. Addressing bytes by
`sha256` requires `blob:cas:read`, the infrastructure path for pipelines
re-reading artifacts — without it, names would be a fence with an open
gate. A put that declares `permissions` must hold them; a put that omits
them keeps the prior ones (never a silent declassify); an explicit `[]`
clears them.

This closes **context mixing** — a guest-lane operation is structurally
unable to read internal names — not malicious authors; declared context
is platform code.

## Seeding blobs with a stack

A stack may ship blobs declaratively in the reserved `BLOBS/` tree; the
tree *is* the hierarchy:

```
OPS/<stack>/
  BLOBS/
    faqs/house-01.md      → blob name "faqs/house-01.md"
    bookings/2026.csv     → blob name "bookings/2026.csv"
```

`BLOBS/` is **data**, deployed by `txco data apply` alongside `VECTORS/`
and `KV/` (and mirrored live by `txco dev`); `txco apply` carries it
forward untouched. Each file is hashed by streaming, probed with
`HEAD /blobs/sha256/{hash}`, streamed up only when missing, and referenced
from the version as a fingerprint row — the same plane as dataset
artifacts, so seeded blobs can be any size. On activation the name index is
pointed at the shipped hashes: an edited file repoints its name, a removed
file unlinks it. **The pack owns exactly the names it ships** — names a
runtime put created are never touched. Seeded names carry no permissions
(content type comes from the extension).

### Seeded names and runtime edits: the git model

The tree is your working copy; the chassis is the remote. Every seeded
name remembers what the tree last shipped, so a runtime `blob/put` that
repoints it — a host editing `faqs/house-01.md` through your app — is
**drift**: the remote has moved past your last push.

```sh
txco data apply          # refused: the chassis has edits your tree doesn't
txco data pull           # bring the live content into BLOBS/ (hash-verified)
txco data apply          # fast-forward: ships the tree; drift clears
txco data apply --force  # the tree wins: re-seeds every name, drops removed ones
```

The refusal is git's non-fast-forward test: a drifted name whose live
content your tree already holds (you pulled, or made the same edit) passes;
a drifted name you deleted from the tree is a conflict. The same rule
applies one layer up, to the stack's version pointer: `data apply` (like
`apply`) refuses when the chassis's active version is no longer the one
this workspace last synced — see the fast-forward rule in [cli](./cli.md).

Without `--force` the chassis never clobbers a runtime edit either: a
seeded name whose tree file is unchanged keeps its runtime content, a name
whose tree file changed takes the tree's new content, and a drifted name
the tree dropped is kept. `txco pull` (the code verb) writes the tree as
last *deployed*; `txco data pull` writes what is *live*.

## Runtime model

Bytes live in the content store every node shares (the bundled local
directory on one machine; the fleet's object store). Names live in the
tenant KV store's reserved `_txc.blob` namespaces, so they are exactly as
shared as the KV store (`--kvstore redis` on a fleet). A put writes the
bytes, then the tenant's ownership record for the hash, then the name —
every crash point leaves something harmless (unreferenced bytes, an
ownership record with no name). Two concurrent puts to one name: the last
writer wins, which *is* replace; `replaced` is best-effort under a race.

Packs and runtime puts share one name space per tenant. Keep the roots
apart (`faqs/…` seeded, `paris/docs/…` runtime) unless you want the
push/pull discipline above to govern those documents.

The store keeps every version of every blob — there is no garbage
collection yet — and listing reads the tenant's whole index per call, which
is right for thousands of names, not millions.

## Limits

| knob | default | what it bounds |
|---|---|---|
| `--blob-max-bytes` | 32 MiB | decoded size of a `blob/put` value and of a `blob/get` read into the envelope (`WITH max_bytes` lowers it per op) |
| `--dataset-max-file-bytes` | 4 GiB | a seeded `BLOBS/` file at upload |
| name length | 250 bytes | the index key |

A put and a get charge fuel per MiB moved on top of the flat per-operation
cost. A 30 MiB upload body stays in the envelope after `blob/put` until
you drop it: `EMIT @delete = &array("@web.req.body")` is allowed for exactly
this (the inbound body is delete-only; it can never be written).

A worked example lives at `examples/blob-store/`.
