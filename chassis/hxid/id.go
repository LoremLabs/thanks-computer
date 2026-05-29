// Package hxid produces sortable, base58-encoded unique IDs backed by ULIDs.
package hxid

import (
	"math/rand"
	"time"

	"github.com/mr-tron/base58"
	"github.com/oklog/ulid/v2"
)

// ID is a 16-byte ULID rendered as base58 via String().
type ID struct{ u ulid.ULID }

var entropy = &ulid.LockedMonotonicReader{
	MonotonicReader: ulid.Monotonic(
		rand.New(rand.NewSource(time.Now().UnixNano())),
		0,
	),
}

// New returns a freshly generated, time-sortable ID.
func New() ID {
	return ID{u: ulid.MustNew(ulid.Timestamp(time.Now()), entropy)}
}

// NewTimeSort is retained for call-site compatibility with the previous
// hxid API. Every ULID is already lexicographically time-sortable, so it
// is an alias for New.
func NewTimeSort() ID { return New() }

// String returns the base58 encoding of the underlying 16-byte ULID.
func (id ID) String() string { return base58.Encode(id.u[:]) }
