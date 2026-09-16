// Package scheduledsink delivers drive mutation events through the
// scheduled store: every committed mutation becomes one pending
// scheduled_events row, due now, that the `scheduled` personality fires
// into the tenant's `_scheduled/0` stack like any other scheduled event
// (`@src == "scheduled"`, the facts under `@scheduled.payload`).
//
// This is the only package that imports both drive and scheduled; drive
// itself knows only the MutationSink interface. With transactional=true the
// row is written on the drive store's own transaction (EnqueueTx), so the
// event commits with the mutation or not at all — valid only when both
// stores are the SAME database, which boot probes before choosing.
package scheduledsink

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/scheduled"
)

// Payload is the scheduled event's payload: a top-level `event`
// discriminator (`drive.resource.<verb>`) and the facts under `drive`, so a
// consumer that multiplexes its `_scheduled` stack on another top-level key
// (`kind`, say) is not shadowed.
type Payload struct {
	Event string `json:"event"`
	Drive Facts  `json:"drive"`
}

// Facts are the mutation's facts as the consumer sees them.
type Facts struct {
	Tenant       string `json:"tenant"`
	CollectionID string `json:"collection_id"`
	Collection   string `json:"collection"`
	ResourceID   string `json:"resource_id"`
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	FromPath     string `json:"from_path,omitempty"`
	ETag         string `json:"etag,omitempty"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type,omitempty"`
	ModSeq       int64  `json:"modseq"`
	At           string `json:"at"`
}

// Encode is the payload of one mutation.
func Encode(m drive.Mutation) json.RawMessage {
	b, _ := json.Marshal(Payload{Event: m.Event, Drive: Facts{
		Tenant: m.Tenant, CollectionID: m.CollectionID, Collection: m.Collection, ResourceID: m.ResourceID,
		Kind: m.Kind, Path: m.Path, FromPath: m.FromPath, ETag: m.ETag, Size: m.Size, ContentType: m.ContentType,
		ModSeq: m.ModSeq, At: m.At.UTC().Format(time.RFC3339),
	}})
	return b
}

type sink struct {
	st *scheduled.Store
}

type txSink struct{ sink }

// New returns a drive.MutationSink over the scheduled store. transactional
// selects the variant that writes inside the drive transaction.
func New(st *scheduled.Store, transactional bool) drive.MutationSink {
	if transactional {
		return txSink{sink{st: st}}
	}
	return sink{st: st}
}

// Enqueue implements drive.MutationSink (after the drive commit).
func (s sink) Enqueue(ctx context.Context, m drive.Mutation) error {
	_, err := s.st.Enqueue(ctx, m.Tenant, drive.IdempotencyKey(m), m.At, Encode(m))
	return err
}

// EnqueueTx implements drive.TransactionalMutationSink (inside the drive
// transaction).
func (s txSink) EnqueueTx(ctx context.Context, tx *sql.Tx, m drive.Mutation) error {
	return s.st.EnqueueTx(ctx, tx, scheduled.NewEventID(), m.Tenant, drive.IdempotencyKey(m), m.At, Encode(m))
}

// Compile-time interface checks.
var (
	_ drive.MutationSink              = sink{}
	_ drive.TransactionalMutationSink = txSink{}
)
