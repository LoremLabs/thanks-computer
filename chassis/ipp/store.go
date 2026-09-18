// Package ipp is the chassis side of the print inlet: the durable job store
// the `ipp` personality writes to, plus the protocol helpers (codec,
// printer attributes, document sniffing) it answers with. The personality
// itself — HTTP, authentication, spooling, dispatch — lives in
// chassis/server/personality/ipp.
//
// The store owns exactly one concern: an IPP job's life from the first byte
// of its document to the moment TxCo accepts responsibility for it.
//
//	receiving  the IPP body is in progress
//	committed  the document is in the CAS, the tenant owns it, this row is durable
//	delivered  the envelope was accepted onto the TxCo execution path
//	canceled   the client canceled before delivery           (terminal)
//	failed     IPP infrastructure could not deliver it       (terminal)
//
// `delivered` is where IPP's responsibility ENDS. What the stack then does
// with the document — create a task, wait three days for a human, fail —
// is not part of a print job's lifecycle, and nothing here observes it.
package ipp

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Job states. See the package doc for the lifecycle.
const (
	StateReceiving = "receiving"
	StateCommitted = "committed"
	StateDelivered = "delivered"
	StateCanceled  = "canceled"
	StateFailed    = "failed"
)

// tsLayout is the row timestamp format: RFC 3339 UTC, which compares
// lexicographically === chronologically (the scheduled store's rule).
const tsLayout = time.RFC3339

// Store is the façade over the job tables. It carries the dialect (for
// `?`→`$n` rebinding and the skip-locked lease) and a clock seam for tests.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
}

// NewStore builds a Store over the opened DB and its dialect. A nil dialect
// defaults to SQLite (the in-tree default).
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and backends. Not for request paths.
func (s *Store) DB() *sql.DB { return s.db }

// SetClock pins the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) ts() string { return s.now().Format(tsLayout) }

// schema is portable DDL: TEXT/BIGINT/INTEGER only, partial indexes (both
// engines support them), additive-only evolution. lease_node/lease_at are a
// dispatcher's claim on a committed row — deliberately columns, not a state:
// a lease is not something an IPP client can observe.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS ipp_jobs (
	   id              TEXT PRIMARY KEY,
	   tenant          TEXT NOT NULL,
	   printer         TEXT NOT NULL,
	   job_number      BIGINT NOT NULL,
	   requesting_user TEXT NOT NULL DEFAULT '',
	   job_name        TEXT NOT NULL DEFAULT '',
	   document_name   TEXT NOT NULL DEFAULT '',
	   document_format TEXT NOT NULL DEFAULT '',
	   sha256          TEXT,
	   size            BIGINT NOT NULL DEFAULT 0,
	   state           TEXT NOT NULL,
	   state_reason    TEXT NOT NULL DEFAULT '',
	   lease_node      TEXT,
	   lease_at        TEXT,
	   attempts        INTEGER NOT NULL DEFAULT 0,
	   rid             TEXT,
	   host            TEXT NOT NULL DEFAULT '',
	   uri_path        TEXT NOT NULL DEFAULT '',
	   client_ip       TEXT NOT NULL DEFAULT '',
	   created_at      TEXT NOT NULL,
	   updated_at      TEXT NOT NULL,
	   committed_at    TEXT,
	   delivered_at    TEXT,
	   UNIQUE (tenant, job_number)
	 )`,
	`CREATE INDEX IF NOT EXISTS ipp_jobs_due_idx ON ipp_jobs (committed_at) WHERE state = 'committed'`,
	`CREATE INDEX IF NOT EXISTS ipp_jobs_printer_idx ON ipp_jobs (tenant, printer, created_at)`,
	`CREATE TABLE IF NOT EXISTS ipp_job_seq (
	   tenant      TEXT PRIMARY KEY,
	   next_number BIGINT NOT NULL
	 )`,
}

// EnsureSchema creates the tables. Idempotent.
func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, q := range schema {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ipp: ensure schema: %w", err)
		}
	}
	// Additive columns, for a table that predates them. CREATE TABLE IF NOT
	// EXISTS leaves an existing table alone, and the two engines disagree
	// about ALTER ... ADD COLUMN IF NOT EXISTS (Postgres has it, SQLite does
	// not), so the portable move is to PROBE and then add (the drive store's
	// idiom).
	for _, c := range []struct{ table, column, ddl string }{
		{"ipp_jobs", "uri_path", `ALTER TABLE ipp_jobs ADD COLUMN uri_path TEXT NOT NULL DEFAULT ''`},
	} {
		if _, err := s.db.ExecContext(ctx, `SELECT `+c.column+` FROM `+c.table+` WHERE 1 = 0`); err == nil {
			continue
		}
		if _, err := s.db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("ipp: add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}
