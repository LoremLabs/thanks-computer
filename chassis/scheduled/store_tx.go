package scheduled

// Tx-accepting variant of Enqueue. A caller that needs its event row to
// commit WITH another store's mutation on the same database (the drive
// store's mutation sink, when --drive-store and --scheduled-store share one
// Postgres) uses EnqueueTx on its own *sql.Tx instead of the s.db-direct
// Enqueue above.
//
// Enqueue remains the canonical surface for callers that don't need that
// atomicity (txco://schedule, tests). Both delegate to the same SQL — the
// only difference is the execer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// execer is implemented by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NewEventID mints a scheduled_events row id (`sched_<hxid>`), for a caller
// that needs the id before EnqueueTx.
func NewEventID() string { return "sched_" + hxid.NewTimeSort().String() }

// EnqueueTx is Enqueue inside the caller's transaction: the row is written
// on tx and lands (or not) with the caller's commit. id is the row id the
// caller minted (NewEventID). Same validation and ON CONFLICT semantics as
// Enqueue.
func (s *Store) EnqueueTx(ctx context.Context, tx *sql.Tx, id, tenant, idempotencyKey string, at time.Time, payload json.RawMessage) error {
	if id == "" {
		return errors.New("scheduled: empty id")
	}
	return s.enqueue(ctx, tx, id, tenant, idempotencyKey, at, payload)
}

// enqueue is the shared body of Enqueue and EnqueueTx.
func (s *Store) enqueue(ctx context.Context, x execer, id, tenant, idempotencyKey string, at time.Time, payload json.RawMessage) error {
	if tenant == "" {
		return errors.New("scheduled: empty tenant")
	}
	if idempotencyKey == "" {
		return errors.New("scheduled: empty idempotency_key")
	}
	// Keys are op-authored and often embed external input (subscriber
	// emails); (tenant, idempotency_key) is a UNIQUE btree key, and on
	// Postgres an index tuple caps at 2704 bytes — an unbounded key fails
	// the INSERT with SQLSTATE 54000 and the drip is silently unscheduled.
	if len(idempotencyKey) > 512 {
		return fmt.Errorf("scheduled: idempotency_key exceeds 512 bytes (%d)", len(idempotencyKey))
	}
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	if !json.Valid(payload) {
		return errors.New("scheduled: payload is not valid JSON")
	}
	// json.Valid does NOT reject invalid UTF-8 inside string literals, but
	// the Postgres TEXT payload column does (SQLSTATE 22021) — and envelopes
	// can carry mail-derived bytes that are not UTF-8. Fail loud here, on
	// both engines, rather than engine-dependently at the INSERT.
	if !utf8.Valid(payload) {
		return errors.New("scheduled: payload is not valid UTF-8")
	}

	now := s.now().Format(time.RFC3339)
	_, err := x.ExecContext(ctx, s.rb(`
		INSERT INTO scheduled_events
			(id, tenant, idempotency_key, schedule_at, payload, status, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?)
		ON CONFLICT (tenant, idempotency_key) DO UPDATE
			SET schedule_at = excluded.schedule_at,
			    payload     = excluded.payload
		  WHERE scheduled_events.status = 'pending'`),
		id, tenant, idempotencyKey, at.UTC().Format(time.RFC3339), string(payload), now)
	if err != nil {
		return fmt.Errorf("scheduled: enqueue: %w", err)
	}
	return nil
}
