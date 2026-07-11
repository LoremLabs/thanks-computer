// Package scheduled is the durable backing store for the chassis
// `scheduled` inlet and the txco://schedule op: a table of future events
// ("run this payload, not before schedule_at") that the scheduled
// personality polls, claims, and fires.
//
// Storage is dialect-aware (registry.Dialect, the same seam the auth
// registry uses); the bundled backend is a SQLite file. The CLAIM is the
// coordination — a node flips a due row pending→claimed with one
// conditional UPDATE and treats rows-affected==1 as "I won", so a single
// claimer fires each event even when several pollers share one table.
package scheduled

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Claimed is a row this node won at ClaimDue: the bits the firing path
// needs to build the `_scheduled/0` envelope.
type Claimed struct {
	ID             string
	Tenant         string
	IdempotencyKey string
	Payload        json.RawMessage
}

// Store is the thin façade over the scheduled_events table. It carries the
// dialect (for `?`→`$n` rebinding + Postgres-only tweaks) and a clock seam
// for tests, mirroring secrets.Store.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect

	// now is a clock seam for tests. Defaults to time.Now().UTC().
	now func() time.Time
}

// NewStore builds a Store over the opened scheduled DB and its dialect.
// A nil dialect defaults to SQLite (the in-tree default).
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// EnsureSchema creates the scheduled_events table + due index if absent. The
// DDL is portable (TEXT timestamps RFC3339, JSON as TEXT, native partial
// index); a backend calls it once at construction.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS scheduled_events (
			id              TEXT PRIMARY KEY,
			tenant          TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			schedule_at     TEXT NOT NULL,
			payload         TEXT NOT NULL,
			status          TEXT NOT NULL DEFAULT 'pending',
			claimed_by      TEXT,
			claimed_at      TEXT,
			fired_at        TEXT,
			attempts        INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			UNIQUE(tenant, idempotency_key)
		)`); err != nil {
		return fmt.Errorf("scheduled: ensure table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS scheduled_events_due_idx ON scheduled_events (schedule_at) WHERE status = 'pending'`); err != nil {
		return fmt.Errorf("scheduled: ensure index: %w", err)
	}
	return nil
}

// Enqueue inserts a pending event, or — if a PENDING row already exists for
// (tenant, idempotency_key) — reschedules it (new schedule_at + payload). A
// row that has already left 'pending' (claimed/done/failed) is immutable: the
// ON CONFLICT update no-ops via the status guard, so a spent key can't be
// resurrected. Returns the row id (the proposed id on a no-op conflict).
func (s *Store) Enqueue(ctx context.Context, tenant, idempotencyKey string, at time.Time, payload json.RawMessage) (string, error) {
	if tenant == "" {
		return "", errors.New("scheduled: empty tenant")
	}
	if idempotencyKey == "" {
		return "", errors.New("scheduled: empty idempotency_key")
	}
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	if !json.Valid(payload) {
		return "", errors.New("scheduled: payload is not valid JSON")
	}

	id := "sched_" + hxid.NewTimeSort().String()
	now := s.now().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO scheduled_events
			(id, tenant, idempotency_key, schedule_at, payload, status, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?)
		ON CONFLICT (tenant, idempotency_key) DO UPDATE
			SET schedule_at = excluded.schedule_at,
			    payload     = excluded.payload
		  WHERE scheduled_events.status = 'pending'`),
		id, tenant, idempotencyKey, at.UTC().Format(time.RFC3339), string(payload), now)
	if err != nil {
		return "", fmt.Errorf("scheduled: enqueue: %w", err)
	}
	return id, nil
}

// Cancel deletes a still-PENDING event for (tenant, idempotency_key). Returns
// true if a row was removed; false (no error) if it was already fired or never
// existed. A claimed/done/failed event is immutable and is left untouched.
func (s *Store) Cancel(ctx context.Context, tenant, idempotencyKey string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rb(
		`DELETE FROM scheduled_events
		  WHERE tenant = ? AND idempotency_key = ? AND status = 'pending'`),
		tenant, idempotencyKey)
	if err != nil {
		return false, fmt.Errorf("scheduled: cancel: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimDue claims up to limit due pending rows (schedule_at <= now) in ONE
// statement and returns the rows this call won. The subselect picks the due
// set, the UPDATE flips it, RETURNING hands back the claimed rows.
//
// The previous shape — SELECT candidates, then one conditional UPDATE per
// row — was correct but paid one network round trip per candidate on a
// shared Postgres store (~5s at 200 due rows, all before the first event
// fired), and competing pollers each burned the full round-trip count
// racing for the same rows. Now: on Postgres the subselect takes FOR UPDATE
// SKIP LOCKED, so concurrent pollers partition the due set without waiting;
// on SQLite the clause is empty and the single statement is atomic under
// the single writer. The redundant status guard on the outer UPDATE is a
// belt for any plan shape — a row can never be claimed twice.
func (s *Store) ClaimDue(ctx context.Context, node string, limit int) ([]Claimed, error) {
	if limit <= 0 {
		limit = 100
	}
	nowStr := s.now().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, s.rb(
		`UPDATE scheduled_events
		    SET status = 'claimed', claimed_by = ?, claimed_at = ?, attempts = attempts + 1
		  WHERE status = 'pending'
		    AND id IN (SELECT id FROM scheduled_events
		                WHERE status = 'pending' AND schedule_at <= ?
		                ORDER BY schedule_at
		                LIMIT ?`+s.dialect.SkipLockedClause()+`)
		  RETURNING id, tenant, idempotency_key, payload`),
		node, nowStr, nowStr, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduled: claim due: %w", err)
	}
	defer rows.Close()
	var won []Claimed
	for rows.Next() {
		var c Claimed
		var pl string
		if err := rows.Scan(&c.ID, &c.Tenant, &c.IdempotencyKey, &pl); err != nil {
			return won, fmt.Errorf("scheduled: scan claimed: %w", err)
		}
		c.Payload = json.RawMessage(pl)
		won = append(won, c)
	}
	if err := rows.Err(); err != nil {
		return won, fmt.Errorf("scheduled: claimed rows: %w", err)
	}
	return won, nil
}

// MarkDone moves a claimed row to the terminal 'done' state.
func (s *Store) MarkDone(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE scheduled_events SET status = 'done', fired_at = ? WHERE id = ? AND status = 'claimed'`),
		s.now().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("scheduled: mark done: %w", err)
	}
	return nil
}

// MarkFailed moves a claimed row to the terminal 'failed' state. Used when a
// dispatched event's pipeline errored or its response timed out — an
// at-most-once bias (it likely ran; don't risk a double-fire by retrying). A
// true crash never reaches here: the row stays 'claimed' for ReclaimStale.
func (s *Store) MarkFailed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE scheduled_events SET status = 'failed', fired_at = ? WHERE id = ? AND status = 'claimed'`),
		s.now().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("scheduled: mark failed: %w", err)
	}
	return nil
}

// ReclaimStale resets rows stuck in 'claimed' past staleAfter back to
// 'pending' so another node retries — crash recovery for a node that died
// after claiming but before MarkDone/MarkFailed. Returns the reset count.
// (RFC3339 UTC timestamps compare lexicographically === chronologically.)
func (s *Store) ReclaimStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := s.now().Add(-staleAfter).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE scheduled_events
		    SET status = 'pending', claimed_by = NULL, claimed_at = NULL
		  WHERE status = 'claimed' AND claimed_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("scheduled: reclaim stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Purge deletes terminal (done/failed) rows older than retention so the table
// doesn't grow without bound. Returns the deleted count.
func (s *Store) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := s.now().Add(-retention).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, s.rb(
		`DELETE FROM scheduled_events
		  WHERE status IN ('done', 'failed') AND COALESCE(fired_at, created_at) < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("scheduled: purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
