# Datasets

A stack can bundle large, read-only lookup data — a book catalog, airport
codes, a GeoIP table — as a SQLite file that deploys WITH the code and is
queried locally at runtime. No external API, no separate lookup service.

## Authoring

Two files under the reserved `DATASETS/` subtree, paired by name:

```
OPS/<stack>/
  DATASETS/
    books.sqlite    the artifact (any schema; FTS5 supported)
    books.yaml      the named queries
```

The manifest declares every query the runtime may execute. Nothing else —
not even a well-formed SELECT — can run against the artifact, and
parameters are always bound:

```yaml
queries:
  search:
    sql: |
      SELECT b.isbn13, b.title, b.author
        FROM books_fts f JOIN books b ON b.rowid = f.rowid
       WHERE books_fts MATCH ?
       ORDER BY rank
    max_rows: 10          # optional; tightens the node cap, never widens
  by_isbn:
    sql: SELECT * FROM books WHERE isbn13 = ?
```

## Querying

```txcl
WHEN @web.req.url.query.q.0 != ""
  EXEC "txco://dataset"
    WITH dataset = "books",
         query = "search",
         args = &array(@web.req.url.query.q.0)
```

The result lands at `into` (default `_dataset`, private to the flow):

```json
{"_dataset": {"dataset": "books", "query": "search",
  "rows": [{"isbn13": "…", "title": "…"}], "count": 1, "truncated": false}}
```

Optional WITH params: `args` (positional binds), `limit` (tightens the row
cap), `stack` (read another of your tenant's stacks), `into`. Errors arrive
as `dataset.error.{code,message}` — handle with `WHEN @dataset.error`.
Codes: `txco_dataset_not_found`, `txco_dataset_unknown_query`,
`txco_dataset_invalid_arg`, `txco_dataset_missing_artifact`,
`txco_dataset_store`, `txco_dataset_query`.

## What apply does

`txco apply` hashes each artifact by streaming (never in memory), asks the
chassis `HEAD /blobs/sha256/{hash}`, streams the bytes only when missing,
and references them from the version as a fingerprint row. Unchanged
artifacts cost one HEAD. `txco pull` streams them back down the same plane,
hash-verified.

Activation is the gate: the artifact must be in the content-addressed
store, the manifest must parse, and every declared query must PREPARE
read-only against the shipped schema. A typo'd column or a sneaky `DELETE`
fails the deploy — with the file and query named — not the request path.

## Runtime model

Artifacts are immutable (the reference is their content hash), so each node
opens them read-only + immutable: no locks, no journal, shared across
requests. Enforcement is layered: only declared queries run; the connection
is `query_only`; and a SQLite authorizer default-denies everything but
reads, so even a write that somehow reached preparation dies with "not
authorized".

Nodes fetch an artifact from the content-addressed store on first use and
keep it in a local disk cache (fleet nodes prefetch on activation). When
the store is the bundled local-disk backend the artifact is opened in
place — zero copies at any size.

## Limits

| knob | default | what it bounds |
|---|---|---|
| `--dataset-max-file-bytes` | 4 GiB | artifact size at upload + activation |
| `--dataset-max-rows` | 200 | rows per query (manifest `max_rows` and WITH `limit` clamp under it) |
| `--dataset-cache-bytes` | 4 GiB | node-local materialise cache (LRU) |
| `--dataset-cache-dir` | `./chassis/data/datasets` | where cached artifacts live |

Responses are additionally capped at 1 MiB of rows (`truncated: true` when
hit). Queries run under the ordinary per-op timeout (`WITH timeout`
raises it, up to the node max).

Superseded artifact versions currently stay in the content-addressed store
(no garbage collection yet); at multi-GB scale, expect storage to grow with
every changed-artifact deploy.

A worked example lives at `examples/dataset-lookup/`.
