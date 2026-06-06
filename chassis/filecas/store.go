// Package filecas is the content-addressed store for tenant FILES/ assets.
// The bytes live here keyed by their sha256; the runtime DB holds only the
// fingerprint (stack_files.content_hash). This keeps tenant file bytes out
// of the in-memory runtime DB on every node and lets identical content
// dedup across tenants.
//
// Like artifact/continuation it is a generic, swappable seam: the backend
// is selected by name through a registry, so an S3-compatible backend can
// register itself (fleet overlay) without changing this package or its
// callers. The bundled "file" backend (sub-package filestore) is disk-only
// and self-registers via a blank import.
//
// Integrity: the key is the content's sha256, and every backend's Put MUST
// Verify(hash, data) before writing — a key is always a truthful digest of
// the bytes, never merely the caller's claim.
//
// Immutability: values are content-addressed and therefore immutable.
// Callers MUST treat bytes returned by Get as read-only; the LRU wrapper
// (cached.go) hands back copies, but a bare backend may return a slice it
// does not expect to be mutated.
package filecas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrNotFound is returned by Get/Exists when a hash is absent.
var ErrNotFound = errors.New("filecas: hash not found")

// ErrHashMismatch is returned by Put when sha256(data) != hash.
var ErrHashMismatch = errors.New("filecas: data does not match hash")

// Store is a content-addressed blob store. The key IS the content's
// lowercase sha256 hex.
type Store interface {
	// Put writes data under hash iff sha256(data)==hash (ErrHashMismatch
	// otherwise) and the hash is absent (create-if-absent dedup; re-Put of
	// identical content is a no-op success).
	Put(ctx context.Context, hash string, data []byte) error
	// Get returns the bytes for hash. ErrNotFound if absent. The result
	// MUST be treated as immutable by the caller.
	Get(ctx context.Context, hash string) ([]byte, error)
	// Exists reports presence without fetching bytes.
	Exists(ctx context.Context, hash string) (bool, error)
	// Name is the backend identity (for logs).
	Name() string
}

// Verify reports ErrHashMismatch unless hash is the lowercase sha256 hex
// of data. Backends call this at the top of Put — never trust the caller's
// hash alone.
func Verify(hash string, data []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != hash {
		return fmt.Errorf("%w: have %s want %s", ErrHashMismatch, got, hash)
	}
	return nil
}

// ShardKey validates a 64-char lowercase sha256 hex and returns the
// maildir-sharded relative key "sha256/<h[:2]>/<h[2:4]>/<h>". ok=false on a
// malformed hash (wrong length or non-hex char) — backends reject it,
// which is the defense against a crafted key escaping the store root.
func ShardKey(hash string) (key string, ok bool) {
	if len(hash) != 64 {
		return "", false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", false
		}
	}
	return "sha256/" + hash[0:2] + "/" + hash[2:4] + "/" + hash, true
}
