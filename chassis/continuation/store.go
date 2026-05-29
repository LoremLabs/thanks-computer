// Package continuation provides durable, immutable, event-sourced storage
// for suspended opstack runs ("continuations").
//
// The model (see internal docs/todo-continuation-storage.md):
//   - Documents are immutable — never rewritten.
//   - Completion writes are append-only or write-once at known keys.
//   - Run/stage state is DERIVED by reading which docs exist.
//   - The only coordination primitive is atomic create-if-absent.
//
// The object store is the source of truth; there is no SQLite for run
// state. A DB, if ever added, is an optional performance index behind this
// same interface.
package continuation

import (
	"context"
	"errors"
	"io"
)

// Ref is a portable reference to a stored object.
type Ref struct {
	Store string `json:"store"`          // backend name: "file" (s3 reserved)
	Key   string `json:"key"`            // logical key, "/"-separated
	Size  int64  `json:"size,omitempty"` // bytes written
}

// Meta is optional object metadata.
type Meta struct {
	ContentType string            `json:"content_type,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

var (
	// ErrExists is returned by Create when the key already exists. This is
	// the load-bearing signal for idempotency: a duplicate write that hits
	// ErrExists is a harmless no-op, and a single successful Create out of
	// a race is the sole "winner" (resume-claim, op-terminal).
	ErrExists = errors.New("continuation: key already exists")

	// ErrNotFound is returned by Get when the key does not exist.
	ErrNotFound = errors.New("continuation: key not found")
)

// Store is the continuation object store. Create is create-if-absent ONLY:
// there is deliberately no overwrite and no compare-and-swap. Immutability
// plus create-if-absent is the entire coordination model.
type Store interface {
	// Create writes data at key iff key does not already exist. Returns
	// ErrExists (and writes nothing) when the key is present. The write is
	// atomic: a reader never observes a partial object.
	Create(ctx context.Context, key string, r io.Reader, meta Meta) (Ref, error)

	// Get returns the full object bytes. ErrNotFound if absent. Bytes are
	// fully buffered — continuation docs and op JSON are bounded.
	Get(ctx context.Context, key string) ([]byte, Meta, error)

	// Exists reports whether key is present. Cheaper than Get for the
	// existence checks that derive run/stage state.
	Exists(ctx context.Context, key string) (bool, error)

	// List returns the keys present under prefix (recursively).
	List(ctx context.Context, prefix string) ([]string, error)

	// Delete removes key. Absent key is not an error (idempotent).
	Delete(ctx context.Context, key string) error

	// Name is the backend identity recorded in Ref.Store.
	Name() string
}
