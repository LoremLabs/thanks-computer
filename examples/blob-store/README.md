# blob-store — runtime blobs via `txco://blob/*`

A stack that keeps runtime-writable bytes under mutable, permissioned
names — its `/var/lib`, where `FILES/` is its `/usr/share`. Two things
happen here:

- `BLOBS/faqs/*.txt` is a **seed tree**: `txco dev` (or `txco data
  apply`) streams each file to the content store and names it on
  activation — `BLOBS/faqs/house-01.txt` becomes the blob
  `faqs/house-01.txt`.
- `POST /blobs/upload` **retains a document at runtime** under a name
  derived from its filename (`docs/<sha256 of the filename>`), marked
  `blob:internal:read`. The guest lane (`/blobs/get`, declared
  `audience = "guest"`) can read the seeded FAQ but answers 403 for the
  internal document — context mixing is impossible by construction.

```
OPS/blob-demo/
  BLOBS/faqs/*.txt        seed tree — streamed via the blob plane, named on activation
  100/list|get|stat|…     one txco://blob/* op per path (guest + internal lanes)
  110/upload_put.txcl     blob/put WITH under + filename (derived name), permissions
  200/*.txcl              responders; denied → 403, other blob.error → 400
```

Run it:

```
txco dev          # from this directory
curl 'http://localhost:8080/blobs/list?prefix=faqs/'
curl 'http://localhost:8080/blobs/get?name=faqs/house-01.txt'
curl -X POST http://localhost:8080/blobs/upload -d '{"filename":"Menu (v2).txt","content_b64":"Um9vZnRvcCBwb29sIGNvZGUgaXMgNzMxMSDigJQgZG8gbm90IHNoYXJlIHdpdGggZ3Vlc3RzLg=="}'
curl 'http://localhost:8080/blobs/get?name=docs/42993b685e9ffdd54b754d17ef41abc8df8a450b8573cd05bfe95bee5822b122'            # 403: guest lane
curl 'http://localhost:8080/blobs/internal/get?name=docs/42993b685e9ffdd54b754d17ef41abc8df8a450b8573cd05bfe95bee5822b122'   # 200
```

Uploading an edited file with the same filename repoints the same name
(`"replaced":true`); the previous bytes stay in the content store,
addressable by sha with `blob:cas:read`.

Seeded names follow the git model. Edit one at runtime and the tree is
behind the chassis:

```
curl -X POST --data-binary 'Bookings now open to walk-ins.' \
  'http://localhost:8080/blobs/edit?name=faqs/bookings.txt'
txco data apply          # refused: faqs/bookings.txt changed at runtime
txco data pull           # writes the live content into BLOBS/faqs/bookings.txt
txco data apply          # ships it; or `--force` to make the tree win instead
```

See `docs/advanced/blobs.md`.
