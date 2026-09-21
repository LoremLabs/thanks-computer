# Search — lexical retrieval as operations

_Thanks, Computer ships a durable, tenant-scoped **lexical search store**: a
place to index text records and find them again by the words they contain,
reached from txcl as ordinary [operations](./ops.md). It is the counterpart of
the [vector store](./vectors.md). Vectors find what a question **means**;
search finds what it **says**: a name, a filename, an invoice number, a quoted
clause._

The whole loop is three ops: **ensure** a collection, **upsert** records, and
later **query** them.

```txcl
# 1. Once: make sure the collection exists.
WITH collection = "handbook"
EXEC "txco://search/collection"
```

```txcl
# 2. Index a record when it arrives. The same id replaces.
WITH collection = "handbook",
     id       = "tickets:0",
     text     = "Ticket TXC-4821 tracks the broken pool pump.",
     name     = "tickets.md",
     title    = "Maintenance tickets",
     metadata = &object("doc", "tickets.md", "audience", "team")
EXEC "txco://search/upsert"
```

```txcl
# 3. Later: find the best records for what somebody wrote.
WITH collection = "handbook",
     query      = @web.req.body.question,
     limit      = 6,
     filter     = &object("audience", &array("anyone", "team")),
     into       = "._hits"
EXEC "txco://search/query"
```

The hits land at the path you name with `into` (default `_search.hits`), best
first, each with `{id, rank, text, metadata}`. There is **no score**. "Best
first" is the contract; an engine's score is not, and it would stop meaning the
same thing the day the ranking improves.

## Writing a query

`query` is plain language. Pass the whole sentence, or the whole email:

- **Words are OR-ed and ranked.** A record need not contain every word, and rare
  words count for more than common ones.
- **A quoted string is a phrase.** `"termination for convenience"` lifts the
  record that holds those words together, in that order.
- **An identifier is matched whole.** `TXC-4821`, `matt@example.com`,
  `foo.bar.baz`, `2026-09-21` and `github.com/foo/bar` each find the record that
  contains exactly that, ahead of records that merely share one of its parts.
- **Case and accents fold.** `CREME BRULEE` finds `crème brûlée`.
- **Nothing is stemmed.** `exercises` does not match `exercise`. That keeps the
  behaviour the same in every language, and never mangles an identifier.
- **No engine syntax is recognised.** `+`, `-`, `*` and `field:` are just
  punctuation, so a query can never be an injection.

A query addresses exactly one collection. To search several, run several
queries in one scope (they run concurrently) and merge above them.

## Records

| Field | |
| --- | --- |
| `id` | Required. Unique within the collection. |
| `text` | The body. Returned on a hit. |
| `title`, `heading`, `name` | Short searched fields. A filename or a title found intact counts for more than the same words in a body. |
| `entities` | A list of names to search, up to 64. |
| `metadata` | An object. **Filtered on, never searched.** Returned on a hit. |

`upsert` takes one record from top-level keys, as above, or a batch as `items`.
A batch is all or nothing. Keys a record carries beyond these are ignored, so
the `items` array a stack builds for `txco://vector/upsert` can be handed to
both ops as it is.

## Collections

A **collection** is one corpus with its own index and its own word statistics.
What one collection holds never moves another's ranking, so give each
independent corpus its own: one per customer, per project, per mailbox.
Collections belong to the tenant, not to a stack, so one stack can index while
another queries.

`txco://search/collection` creates the collection if it is missing and
describes it at `_search.collection`:

```json
{ "name": "handbook", "analyzer_version": "txco_v1", "scoring_model": "bm25", "records": 4 }
```

The analyzer and scoring model are pinned when the collection is created.
Passing `analyzer_version` asserts the pin; a mismatch is
`txco_search_analyzer_mismatch`.

## Filters

`filter` is the same grammar `txco://vector/search` takes, so **one filter
object narrows both retrieval lanes**:

```txcl
filter = &object(
  "audience", "anyone",                        # scalar     → equals
  "doc",      &array("faq.md", "parking.md"),  # array      → any of
  "idx",      &object("gte", 3),               # op-object  → gte, lte, gt, lt
  "id",       &object("not_in", ._seen))       # op-object  → not_in; `id` is the record id
```

Conditions are AND-ed. Matching is exact and typed: the string `"3"` is not the
number `3`. A record that lacks a field passes `not_in` for it. A filter admits
or rejects and never moves the order.

## Changing and removing records

```txcl
# A file was renamed, not rewritten: fix its labels, keep its text.
WITH collection = "handbook",
     filter = &object("doc", "parking.md"),
     merge  = &object("doc", "garage.md"),     # metadata keys; a null removes one
     fields = &object("name", "garage.md")     # name, title, heading
EXEC "txco://search/update"
```

`update` touches every record the filter admits and reports how many at `into`
(default `_search.updated`). The filter must carry at least one condition. It
never changes `text`: new text is a new `upsert`.

```txcl
# Remove a whole document without knowing how many chunks it has.
WITH collection = "handbook", filter = &object("doc", "contract.md")
EXEC "txco://search/delete"
```

`delete` takes `ids` (or a single `id`), or a `filter` with at least one
condition, and reports how many records it removed at `into` (default
`_search.deleted`). Removing what is already gone is not an error.

## With the vector store

The two lanes fail differently, which is why they are worth fusing. Run both
queries in one scope, then merge the two ranked lists in the next. Reciprocal
rank fusion is a good first merge: score each id by the sum of `1 / (60 + rank)`
over the lists it appears in. Fusion is the stack's business, and deliberately
not an op: it is policy about your corpus.

## Errors

Every failure is data at `search.error` on the envelope root, a `code` and a
`message`, never a crashed rule:

```txcl
WHEN .search.error.code == "txco_search_disabled"
  EMIT ._lexical = "off"
```

| Code | |
| --- | --- |
| `txco_search_disabled` | This chassis runs without a search store (`--search-store=none`). A stack that fuses two lanes should carry on with one. |
| `txco_search_no_tenant` | No tenant in the request scope. |
| `txco_search_invalid_arg` | A missing `collection`, an unknown filter op, an `update` with no filter, a record with no `id`. |
| `txco_search_collection_not_found` | Ensure it first with `txco://search/collection`. |
| `txco_search_too_large` | Past a limit below. |
| `txco_search_analyzer_mismatch` | The collection is pinned to another analyzer. Re-index it. |
| `txco_search_unavailable`, `txco_search_recovering` | A remote backend cannot answer right now. Treat it as "no lexical results". |
| `txco_search_store` | Anything else the backend reported. |

An empty result is `[]`, never an error.

## Limits

| | |
| --- | --- |
| `text` per record | 64 KiB |
| `title`, `heading`, `name` | 1 KiB each |
| `entities` | 64, of 256 bytes each |
| `metadata` | 16 KiB, 64 keys |
| records per `upsert` | 500 |
| `ids` per `delete` | 500 |
| `query` | 8 KiB; its first 64 terms are used |
| `limit` | default 10, at most 100 |

## Where the data lives

The bundled backend is [Bleve](https://blevesearch.com), one index per
collection under `--search-path` (default `./chassis/data/search`, and
`.txco/dev/search` under `txco dev`). Directory names are hashes; no tenant or
collection name reaches the filesystem. Only recently used indexes stay open
(`--search-max-open-indexes`, default 64); the rest reopen on their next use,
in about 20 ms for a thousand records.

An index is a **projection** of records your stack can produce again, not the
only copy of anything. Back it up with the rest of the data directory, or
rebuild it by indexing again.

The runnable version of this page is
[`examples/search-hello`](../examples/search-hello).
