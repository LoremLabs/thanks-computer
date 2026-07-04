# dataset-lookup — bundled SQLite datasets via `txco://dataset`

A stack that ships its own lookup data: `DATASETS/books.sqlite` (a tiny
book catalog with an FTS5 index) plus `DATASETS/books.yaml` (the named
queries the runtime may execute against it). No external API, no separate
lookup service — the data deploys with the code.

```
OPS/dataset-demo/
  DATASETS/
    books.sqlite   the artifact — content-addressed, streamed via the
                   blob plane, opened read-only + immutable per node
    books.yaml     the manifest — the ONLY SQL that can ever run
  100/lookup.txcl  EXEC "txco://dataset" WITH query="search", args=[?q]
  200/respond.txcl serialize {count, books} (or the structured error)
```

Run it:

```
txco dev          # from this directory
curl 'http://localhost:8080/books?q=angels'
curl 'http://localhost:8080/books?q=hemingway'
```

What `txco apply` does with the pair: hashes the artifact locally
(streaming), probes `HEAD /blobs/sha256/{hash}`, streams the bytes only if
the chassis lacks them, and references them from the draft as a
fingerprint-only row. Activation refuses the version unless the artifact
is present, the manifest parses, and every declared query prepares
read-only against the shipped schema — a typo'd column or a sneaky
`DELETE` fails the deploy, not the request path.

To change the dataset, edit and re-run the generator:

```
sqlite3 OPS/dataset-demo/DATASETS/books.sqlite  # or rebuild from scratch
```

Any re-apply uploads only when the content hash changed.
