package drive

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// Mutation event names: `drive.resource.<verb>`, one per committed
// mutation of a resource. A subtree operation (move, copy or delete of a
// directory) reports ONE event, for the directory; descendants are
// re-derived by listing.
const (
	EventResourceCreated = "drive.resource.created"
	EventResourceUpdated = "drive.resource.updated"
	EventResourceDeleted = "drive.resource.deleted"
	EventResourceMoved   = "drive.resource.moved"
)

// Mutation is the fact a committed mutation reports to the sink: enough
// for a consumer to decide whether to fetch (kind, size, content_type,
// etag) and to find the bytes (collection + resource_id), without the
// bytes themselves riding the event.
type Mutation struct {
	Event        string
	Tenant       string
	CollectionID string
	Collection   string // the collection's name
	ResourceID   string
	Kind         string
	Path         string
	FromPath     string // moved: the previous path
	ETag         string
	Size         int64
	ContentType  string
	ModSeq       int64
	At           time.Time
}

// IdempotencyKey is the mutation's de-duplication key for a queue that
// coalesces on it: `drive:<resource_id>:<modseq>`. Never the resource id
// alone — the scheduled store overwrites a still-pending row on key
// conflict, and two quick writes to one resource must both be delivered.
func IdempotencyKey(m Mutation) string {
	return "drive:" + m.ResourceID + ":" + strconv.FormatInt(m.ModSeq, 10)
}

// MutationSink receives every committed mutation. Enqueue is called AFTER
// the index transaction committed; an error is reported to the store's
// sink-error handler and does not undo the mutation.
type MutationSink interface {
	Enqueue(ctx context.Context, m Mutation) error
}

// TransactionalMutationSink is a sink that can write inside the index
// transaction (the scheduled store on the same database). The store
// prefers EnqueueTx when the sink implements it: the event row then
// commits with the mutation or not at all.
type TransactionalMutationSink interface {
	MutationSink
	EnqueueTx(ctx context.Context, tx *sql.Tx, m Mutation) error
}

// NopSink drops every mutation (the default; a node without a scheduled
// store).
type NopSink struct{}

// Enqueue implements MutationSink.
func (NopSink) Enqueue(context.Context, Mutation) error { return nil }

// emit is the store's single call site: inside the tx for a transactional
// sink (tx non-nil, before commit), after commit otherwise (tx nil).
func (s *Store) emitTx(ctx context.Context, tx *sql.Tx, m Mutation) error {
	if ts, ok := s.sink.(TransactionalMutationSink); ok {
		return ts.EnqueueTx(ctx, tx, m)
	}
	return nil
}

func (s *Store) emitPost(ctx context.Context, m Mutation) {
	if _, ok := s.sink.(TransactionalMutationSink); ok {
		return // already written inside the tx
	}
	if err := s.sink.Enqueue(ctx, m); err != nil {
		s.onSinkErr(m, err)
	}
}
