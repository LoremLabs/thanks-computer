// Package artifact is the content-addressed artifact store: where snapshot
// (and future event) artifacts live. An event in the control contract is a
// notification + a reference into this store; the bytes never travel in the
// event log itself (see internal docs/todo-architecture-saas-fleet.md §3.1).
//
// It deliberately stores OPAQUE bytes plus an OPAQUE manifest blob. It does
// not import chassis/snapshot — callers marshal their own manifest. This
// keeps the store a generic, swappable seam: the backend is selected by
// name through a registry, so an additional backend can be added without
// changing this package or its callers.
package artifact

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get/stat when a ref is absent.
var ErrNotFound = errors.New("artifact: ref not found")

// Store is the artifact backend. Refs are "/"-separated logical keys
// (e.g. "stacks/t_123/web/42"). Each ref holds a data blob and a manifest
// blob, written atomically together (a reader never sees a half-written
// pair). Artifacts are immutable by convention — a ref identifies content —
// but Put does not enforce create-if-absent: re-Putting identical content is
// a harmless idempotent overwrite.
type Store interface {
	// Put writes data + manifest at ref atomically.
	Put(ctx context.Context, ref string, data, manifest []byte) error

	// Get returns the data and manifest blobs. ErrNotFound if absent.
	Get(ctx context.Context, ref string) (data, manifest []byte, err error)

	// Exists reports whether ref is present.
	Exists(ctx context.Context, ref string) (bool, error)

	// Name is the backend identity (for logs).
	Name() string
}
