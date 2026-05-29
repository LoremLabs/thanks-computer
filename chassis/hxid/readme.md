# Globally Unique ID Generator

Thin wrapper around [`github.com/oklog/ulid/v2`](https://pkg.go.dev/github.com/oklog/ulid/v2) that renders the 16-byte ULID as base58 instead of the native Crockford base32. ULIDs are lexicographically time-sortable, so `New()` and `NewTimeSort()` are aliases retained for call-site compatibility.
