# Vectors — semantic retrieval as operations

_Thanks, Computer ships a durable, tenant-scoped **vector store**: a place to
keep embeddings and find nearest neighbours, reached from txcl as ordinary
[operations](./ops.md). Producing the vectors is [`ai://embed`](./ai.md#embeddings--exec-aiembed)'s
job; storing and searching them is `txco://vector/*`. The split is deliberate —
embedding belongs to AI, storage belongs to infrastructure._

The whole loop is three ops: **embed** a piece of text, **upsert** the vector,
and later **search** for the nearest matches to a query.

```txcl
# 1. Embed and store a document (once, when it arrives).
WITH provider = "openai", model = "text-embedding-3-small",
     text = "Winnie-the-Pooh: gentle adventures in the Hundred Acre Wood"
EXEC "ai://embed"

WITH collection = "books",
     id         = "pooh",
     vector     = @_embed.vector,
     metadata   = &object("audience", "children", "public_domain", true),
     text       = "Winnie-the-Pooh: gentle adventures in the Hundred Acre Wood"
EXEC "txco://vector/upsert"
```

```txcl
# 2. Later: embed a query and find the closest books.
WITH provider = "openai", model = "text-embedding-3-small",
     text = @web.req.body.query
EXEC "ai://embed"

WITH collection = "books",
     vector     = @_embed.vector,
     limit      = 5,
     into       = "._hits"
EXEC "txco://vector/search"
```

The matches land at the path you name with `into` (default `_vector.matches`),
each with `{id, score, distance, metadata, text}` — `score` is the normalised
similarity (higher = closer), ready for a reranker or a reply template.

## Collections

A **collection** pins a vector space: an embedding model, a dimension count, and
a distance metric (cosine, in v1). Vectors are only comparable inside one, so the
store rejects an upsert whose dimensions don't match — a model swap is a *new*
collection plus a re-embed, never a silent mix.

Collections are created on first use, or explicitly:

```txcl
WITH collection      = "books",
     embedding_model = "text-embedding-3-small",
     dimensions      = 1536,
     metric          = "cosine"
EXEC "txco://vector/collection"
```

Collections are **tenant-scoped** and shared across a tenant's stacks (unlike a
KV namespace, which defaults to the stack). The tenant is taken from the trusted
request scope, never from the envelope.

## Searching

`txco://vector/search` takes the query `vector`, a `limit`, and an optional
metadata `filter` pushed into the store *before* ranking — so a tight filter both
narrows results and dodges the "nearest 10, but only 2 pass the filter" trap:

```txcl
WITH collection = "books",
     vector     = @_embed.vector,
     limit      = 3,
     filter     = &object("public_domain", true,
                          "id", &object("not_in", @._already_read)),
     into       = "._hits"
EXEC "txco://vector/search"
```

A filter value is matched against item metadata: a scalar means `eq`, an array
means `in`, and an object like `{"not_in": [...]}` / `{"gte": 12}` selects the
operator (`eq`, `in`, `not_in`, `gte`, `lte`, `gt`, `lt`). The special field
`id` filters the item id itself — the idiomatic way to exclude already-seen
results. The other ops are `txco://vector/upsert` (one item, or `items = [ … ]`
in bulk) and `txco://vector/delete` (`ids = [ … ]`). Errors surface at
`vector.error`.

## Deploying data: code vs. data

A search collection is usually a **known set that belongs with the deploy** — a
product catalog, a knowledge base — not something you POST in by hand. Thanks,
Computer treats that data declaratively, in a reserved tree beside `OPS/` and
`FILES/`:

```
OPS/<stack>/
  0100_…/…            # operations  ─┐
  FILES/…             # static assets │ code
  VECTORS/books.jsonl # a collection ─┐
  KV/config.jsonl     # a namespace   │ data
```

**Code and data deploy separately, and data is opt-in:**

- **`txco apply`** (and `txco push`) deploy **code only** — operations and
  `FILES/`. They never touch the stores, and they carry any existing `VECTORS/` +
  `KV/` packs forward untouched. A teammate can check out the repo *without* the
  big data packs, fix an operation, and `txco apply` — the live catalog is
  undisturbed.
- **`txco data apply`** deploys the `VECTORS/` + `KV/` packs: it carries the
  stack's code forward, replaces the data, and reconciles it into the stores.
  (Deploy code first — a stack must already have an active version.)

```sh
txco apply         # ship an operation change; data left alone
txco data apply    # ship the catalog; reconciled into the vector + KV stores
```

Reconcile is **change-driven**: only packs whose contents actually changed since
the last deploy touch the store, so a code deploy does zero data work and a data
re-apply only re-embeds what moved. Within a pack the store is synced to match
(an id you removed from `books.jsonl` is deleted from the collection) — so keep a
seeded collection *pure*: don't also write to it at runtime.

### The pack format

A `VECTORS/<collection>.jsonl` pack is NDJSON, one item per line, with
**pre-computed** vectors — `apply` stays offline and deterministic, so the
vectors are embedded at build time and serialized into the pack:

```json
{"id":"pooh","vector":[0.0123,-0.0456, …],"metadata":{"public_domain":true},"text":"…","model":"text-embedding-3-small"}
```

The optional `model` field pins the collection's embedding model, so the query
path (`ai://embed` with the same model) stays comparable. A `KV/<namespace>.jsonl`
pack is the same idea for key-value seed data: `{"key":"…","value":<any JSON>,"ttl":<seconds?>}`.

:::note
A seeded `KV/` namespace is synced to match its pack, so it must be a namespace
**no runtime op writes** — never a stack's default namespace (which is the stack
name). Keep seed config in a dedicated namespace like `KV/config.jsonl`.
:::

## Inspecting and tearing down

The `txco data` verbs show what's live and what a re-apply would change:

```sh
txco data ls                       # collections, with model / dims / item count
txco data show books               # one collection's pin + item ids
txco data diff books VECTORS/books.jsonl   # what `data apply` would add / remove
txco data rm books --yes           # drop a whole collection (explicit; apply never does this)
```

Removing a pack file from your tree **stops managing** its collection — it does
not delete it. Tearing a collection down is the deliberate `txco data rm`.

## Backends

The store is durable and bounded — never an in-process index that a restart
reloads or a large upload could OOM. Two backends sit behind one interface:

- **Bundled** — SQLite + [sqlite-vec](https://github.com/asg017/sqlite-vec) in a
  dedicated file (`--vector-db-path`). Per-node: each node seeds its own copy
  from the deploy. Great for a read-only catalog; the default.
- **Shared (HA)** — PostgreSQL + `pgvector`, set with `--vector-postgres-dsn`.
  One store across every node, so runtime writes and seeds are visible fleet-wide;
  the seed reconciles once, on the control plane.

Both are exact (brute-force) nearest-neighbour in v1 — correct, and fine into the
~100k-vector range; the crossover to approximate search is scale or HA, not
catalog size. Embeddings need a provider key in prod (the `OPENAI_KEY` secret for
the OpenAI backend); see [AI § embeddings](./ai.md#embeddings--exec-aiembed).
