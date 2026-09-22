package state

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

// Store is the façade over the two tables. It carries the dialect (for
// `?`→`$n` rebinding), the data cap, and a clock seam for tests,
// mirroring notebook.Store.
type Store struct {
	db           *sql.DB
	dialect      registry.Dialect
	now          func() time.Time
	maxDataBytes int
}

// NewStore builds a Store over the opened DB and its dialect. A nil
// dialect defaults to SQLite (the in-tree default).
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }, maxDataBytes: DefaultMaxDataBytes}
}

// SetMaxDataBytes installs the node's cap on `data` (0 = uncapped).
func (s *Store) SetMaxDataBytes(n int) {
	if n < 0 {
		n = 0
	}
	s.maxDataBytes = n
}

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and backends. Not for request paths.
func (s *Store) DB() *sql.DB { return s.db }

// EnsureSchema creates the tables + indexes if absent. Portable DDL (the
// notebook store's rules): TEXT ids, TEXT fixed-width timestamps, JSON as
// TEXT, BIGINT counters, native partial indexes — one statement set
// serves SQLite and Postgres. There is no migration runner for these
// stores; changes must be additive.
func (s *Store) EnsureSchema(ctx context.Context) error {
	return registry.RetrySchema(ctx, s.ensureSchema)
}

func (s *Store) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS state_records (
			tenant     TEXT NOT NULL,
			machine    TEXT NOT NULL,
			id         TEXT NOT NULL,
			state      TEXT NOT NULL,
			version    BIGINT NOT NULL,
			data       TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (tenant, machine, id)
		)`,
		`CREATE TABLE IF NOT EXISTS state_events (
			event_id        TEXT PRIMARY KEY,
			tenant          TEXT NOT NULL,
			machine         TEXT NOT NULL,
			record_id       TEXT NOT NULL,
			version         BIGINT NOT NULL,
			from_state      TEXT NOT NULL,
			to_state        TEXT NOT NULL,
			cause_source    TEXT NOT NULL DEFAULT '',
			cause_stack     TEXT NOT NULL DEFAULT '',
			cause_trace     TEXT NOT NULL DEFAULT '',
			cause_run       TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			status          TEXT NOT NULL DEFAULT 'pending',
			attempts        BIGINT NOT NULL DEFAULT 0,
			next_attempt_at TEXT NOT NULL,
			claimed_by      TEXT,
			claimed_at      TEXT,
			delivered_at    TEXT,
			delivered_rid   TEXT,
			last_error      TEXT NOT NULL DEFAULT '',
			UNIQUE (tenant, machine, record_id, version)
		)`,
		`CREATE INDEX IF NOT EXISTS state_events_due_idx
			ON state_events (next_attempt_at) WHERE status = 'pending'`,
		`CREATE INDEX IF NOT EXISTS state_events_claimed_idx
			ON state_events (claimed_at) WHERE status = 'claimed'`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("state: ensure schema: %w", err)
		}
	}
	return nil
}

// --- records ------------------------------------------------------------

const recordCols = `machine, id, state, version, data, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanRecord(r rowScanner, tenant string) (Record, error) {
	var rec Record
	var data, created, updated string
	if err := r.Scan(&rec.Machine, &rec.ID, &rec.State, &rec.Version, &data, &created, &updated); err != nil {
		return Record{}, err
	}
	rec.Tenant = tenant
	rec.Data = json.RawMessage(data)
	rec.CreatedAt = parseAt(created)
	rec.UpdatedAt = parseAt(updated)
	return rec, nil
}

// Create stores a record at version 1. A record already at (tenant,
// machine, id) is an ExistsError carrying what is there; nothing is
// changed and no event is written (creation is not a transition).
func (s *Store) Create(ctx context.Context, req CreateReq) (Record, error) {
	if err := validTenant(req.Tenant); err != nil {
		return Record{}, err
	}
	if err := ValidMachine(req.Machine); err != nil {
		return Record{}, err
	}
	if err := ValidID(req.ID); err != nil {
		return Record{}, err
	}
	if err := ValidState("state", req.State); err != nil {
		return Record{}, err
	}
	data := req.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	if err := validData(data, s.maxDataBytes); err != nil {
		return Record{}, err
	}
	now := formatAt(s.now())
	res, err := s.db.ExecContext(ctx, s.rb(
		`INSERT INTO state_records (tenant, machine, id, state, version, data, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?)
		 ON CONFLICT (tenant, machine, id) DO NOTHING`),
		req.Tenant, req.Machine, req.ID, req.State, string(data), now, now)
	if err != nil {
		return Record{}, &StoreError{Op: "create", Err: err}
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, gerr := s.Get(ctx, req.Tenant, req.Machine, req.ID)
		if gerr != nil {
			return Record{}, gerr
		}
		return Record{}, &ExistsError{Current: cur}
	}
	return s.Get(ctx, req.Tenant, req.Machine, req.ID)
}

// Get reads a record; NotFoundError when there is none.
func (s *Store) Get(ctx context.Context, tenant, machine, id string) (Record, error) {
	if err := validTenant(tenant); err != nil {
		return Record{}, err
	}
	if err := ValidMachine(machine); err != nil {
		return Record{}, err
	}
	if err := ValidID(id); err != nil {
		return Record{}, err
	}
	return s.get(ctx, s.db, tenant, machine, id)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) get(ctx context.Context, q querier, tenant, machine, id string) (Record, error) {
	rec, err := scanRecord(q.QueryRowContext(ctx, s.rb(
		`SELECT `+recordCols+` FROM state_records WHERE tenant = ? AND machine = ? AND id = ?`),
		tenant, machine, id), tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, &NotFoundError{Machine: machine, ID: id}
	}
	if err != nil {
		return Record{}, &StoreError{Op: "get", Err: err}
	}
	return rec, nil
}

// Transition is the compare-and-swap. In one write transaction it (1)
// updates the record only where state = From AND version =
// ExpectedVersion, (2) classifies a miss against the current row
// (NotFoundError; ConflictError "version" first, then "state"), (3)
// inserts the event for the new version, and (4) commits. The event and
// the new version are visible together or not at all: a crash between
// them leaves neither.
//
// Under Postgres READ COMMITTED a second UPDATE racing on the same
// version blocks on the row, re-evaluates against the winner's new
// version and matches nothing; on SQLite the _txlock=immediate DSN
// serializes the two transactions outright. The CAS holds without
// SERIALIZABLE.
func (s *Store) Transition(ctx context.Context, req TransitionReq) (Record, Event, error) {
	if err := validTenant(req.Tenant); err != nil {
		return Record{}, Event{}, err
	}
	if err := ValidMachine(req.Machine); err != nil {
		return Record{}, Event{}, err
	}
	if err := ValidID(req.ID); err != nil {
		return Record{}, Event{}, err
	}
	if err := ValidState("from", req.From); err != nil {
		return Record{}, Event{}, err
	}
	if err := ValidState("to", req.To); err != nil {
		return Record{}, Event{}, err
	}
	if req.ExpectedVersion < 1 {
		return Record{}, Event{}, &InvalidArgError{Reason: "expected_version must be at least 1"}
	}
	if req.Data != nil {
		if err := validData(req.Data, s.maxDataBytes); err != nil {
			return Record{}, Event{}, err
		}
	}

	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Record{}, Event{}, &StoreError{Op: "transition: begin", Err: err}
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	nowStr := formatAt(now)
	var row *sql.Row
	if req.Data != nil {
		row = tx.QueryRowContext(ctx, s.rb(
			`UPDATE state_records SET state = ?, version = version + 1, data = ?, updated_at = ?
			  WHERE tenant = ? AND machine = ? AND id = ? AND state = ? AND version = ?
			  RETURNING `+recordCols),
			req.To, string(req.Data), nowStr, req.Tenant, req.Machine, req.ID, req.From, req.ExpectedVersion)
	} else {
		row = tx.QueryRowContext(ctx, s.rb(
			`UPDATE state_records SET state = ?, version = version + 1, updated_at = ?
			  WHERE tenant = ? AND machine = ? AND id = ? AND state = ? AND version = ?
			  RETURNING `+recordCols),
			req.To, nowStr, req.Tenant, req.Machine, req.ID, req.From, req.ExpectedVersion)
	}
	rec, err := scanRecord(row, req.Tenant)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing matched: say why, against the row as it is now.
		cur, gerr := s.get(ctx, tx, req.Tenant, req.Machine, req.ID)
		if gerr != nil {
			return Record{}, Event{}, gerr
		}
		if cur.Version != req.ExpectedVersion {
			return Record{}, Event{}, &ConflictError{Reason: "version", Current: cur}
		}
		return Record{}, Event{}, &ConflictError{Reason: "state", Current: cur}
	}
	if err != nil {
		return Record{}, Event{}, &StoreError{Op: "transition: update", Err: err}
	}

	ev := Event{
		ID:            "stev_" + hxid.NewTimeSort().String(),
		Tenant:        req.Tenant,
		Machine:       req.Machine,
		RecordID:      req.ID,
		Version:       rec.Version,
		From:          req.From,
		To:            req.To,
		Cause:         req.Cause,
		CreatedAt:     now,
		Status:        StatusPending,
		NextAttemptAt: now,
	}
	if _, err := tx.ExecContext(ctx, s.rb(
		`INSERT INTO state_events (event_id, tenant, machine, record_id, version, from_state, to_state,
		                           cause_source, cause_stack, cause_trace, cause_run,
		                           created_at, status, attempts, next_attempt_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?)`),
		ev.ID, ev.Tenant, ev.Machine, ev.RecordID, ev.Version, ev.From, ev.To,
		ev.Cause.Source, ev.Cause.Stack, ev.Cause.Trace, ev.Cause.Run,
		nowStr, nowStr); err != nil {
		return Record{}, Event{}, &StoreError{Op: "transition: event", Err: err}
	}
	if err := tx.Commit(); err != nil {
		return Record{}, Event{}, &StoreError{Op: "transition: commit", Err: err}
	}
	return rec, ev, nil
}

// --- events (the dispatcher's side) --------------------------------------

const eventCols = `event_id, tenant, machine, record_id, version, from_state, to_state,
	cause_source, cause_stack, cause_trace, cause_run, created_at, status, attempts,
	next_attempt_at, COALESCE(claimed_by, ''), COALESCE(claimed_at, ''),
	COALESCE(delivered_at, ''), COALESCE(delivered_rid, ''), last_error`

func scanEvent(r rowScanner) (Event, error) {
	var ev Event
	var created, next, claimedAt, deliveredAt string
	if err := r.Scan(&ev.ID, &ev.Tenant, &ev.Machine, &ev.RecordID, &ev.Version, &ev.From, &ev.To,
		&ev.Cause.Source, &ev.Cause.Stack, &ev.Cause.Trace, &ev.Cause.Run, &created, &ev.Status, &ev.Attempts,
		&next, &ev.ClaimedBy, &claimedAt, &deliveredAt, &ev.DeliveredRid, &ev.LastError); err != nil {
		return Event{}, err
	}
	ev.CreatedAt = parseAt(created)
	ev.NextAttemptAt = parseAt(next)
	ev.ClaimedAt = parseAt(claimedAt)
	ev.DeliveredAt = parseAt(deliveredAt)
	return ev, nil
}

// GetEvent reads one event by id; NotFoundError when there is none.
func (s *Store) GetEvent(ctx context.Context, eventID string) (Event, error) {
	ev, err := scanEvent(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+eventCols+` FROM state_events WHERE event_id = ?`), eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, &NotFoundError{ID: eventID}
	}
	if err != nil {
		return Event{}, &StoreError{Op: "get event", Err: err}
	}
	return ev, nil
}

// EventsForRecord lists a record's events in version order (tests,
// inspection).
func (s *Store) EventsForRecord(ctx context.Context, tenant, machine, id string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT `+eventCols+` FROM state_events
		  WHERE tenant = ? AND machine = ? AND record_id = ? ORDER BY version`), tenant, machine, id)
	if err != nil {
		return nil, &StoreError{Op: "events for record", Err: err}
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return out, &StoreError{Op: "scan event", Err: err}
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return out, &StoreError{Op: "event rows", Err: err}
	}
	return out, nil
}

// ClaimDue atomically moves up to limit due pending events to 'claimed'
// for node and returns them — one statement, so concurrent claimers
// (SKIP LOCKED on Postgres; the single writer on SQLite) partition the
// due set instead of double-claiming.
//
// Presentation order per record: an event is due only when no earlier
// version of the same record is still pending or claimed. Version 9 is
// never presented until version 8 has reached a terminal status (done,
// skipped or dead), even across nodes. Their RUNS may still overlap,
// because acceptance is not completion; handlers compare versions.
//
// Attempts are not consumed here: the outcome methods decide what
// counted (see Event.Attempts).
func (s *Store) ClaimDue(ctx context.Context, node string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	nowStr := formatAt(s.now())
	rows, err := s.db.QueryContext(ctx, s.rb(
		`UPDATE state_events
		    SET status = 'claimed', claimed_by = ?, claimed_at = ?
		  WHERE status = 'pending'
		    AND event_id IN (SELECT e.event_id FROM state_events e
		                      WHERE e.status = 'pending' AND e.next_attempt_at <= ?
		                        AND NOT EXISTS (SELECT 1 FROM state_events p
		                                         WHERE p.tenant = e.tenant AND p.machine = e.machine
		                                           AND p.record_id = e.record_id
		                                           AND p.version < e.version
		                                           AND p.status IN ('pending', 'claimed'))
		                      ORDER BY e.next_attempt_at, e.version
		                      LIMIT ?`+s.dialect.SkipLockedClause()+`)
		  RETURNING `+eventCols),
		node, nowStr, nowStr, limit)
	if err != nil {
		return nil, &StoreError{Op: "claim due", Err: err}
	}
	defer rows.Close()
	var won []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return won, &StoreError{Op: "scan claimed", Err: err}
		}
		won = append(won, ev)
	}
	if err := rows.Err(); err != nil {
		return won, &StoreError{Op: "claimed rows", Err: err}
	}
	return won, nil
}

// MarkDone closes a claim: the chassis accepted the event for execution
// in the tenant (rid is the run's). Terminal.
func (s *Store) MarkDone(ctx context.Context, eventID, rid string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE state_events SET status = 'done', delivered_at = ?, delivered_rid = ?, last_error = ''
		  WHERE event_id = ? AND status = 'claimed'`),
		formatAt(s.now()), rid, eventID)
	if err != nil {
		return &StoreError{Op: "mark done", Err: err}
	}
	return nil
}

// MarkSkipped closes a claim without a presentation: the tenant has no
// active _state stack, or the run never left _sys. Terminal; not replayed
// if a stack appears later.
func (s *Store) MarkSkipped(ctx context.Context, eventID, reason string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE state_events SET status = 'skipped', delivered_at = ?, last_error = ?
		  WHERE event_id = ? AND status = 'claimed'`),
		formatAt(s.now()), reason, eventID)
	if err != nil {
		return &StoreError{Op: "mark skipped", Err: err}
	}
	return nil
}

// Retry returns a claim to pending, due again after backoff. With
// consumeAttempt the presentation counted (an accept timeout: the run
// may have accepted late, so this is where duplicates come from) and,
// at maxAttempts, the event is dead instead. Without it (an admission
// denial, a refused handoff) nothing was presented and nothing is counted.
func (s *Store) Retry(ctx context.Context, eventID string, backoff time.Duration, reason string, consumeAttempt bool, maxAttempts int) error {
	now := s.now()
	var err error
	if consumeAttempt {
		_, err = s.db.ExecContext(ctx, s.rb(
			`UPDATE state_events
			    SET status = CASE WHEN attempts + 1 >= ? THEN 'dead' ELSE 'pending' END,
			        attempts = attempts + 1, next_attempt_at = ?, last_error = ?,
			        claimed_by = NULL, claimed_at = NULL
			  WHERE event_id = ? AND status = 'claimed'`),
			maxAttempts, formatAt(now.Add(backoff)), reason, eventID)
	} else {
		_, err = s.db.ExecContext(ctx, s.rb(
			`UPDATE state_events
			    SET status = 'pending', next_attempt_at = ?, last_error = ?,
			        claimed_by = NULL, claimed_at = NULL
			  WHERE event_id = ? AND status = 'claimed'`),
			formatAt(now.Add(backoff)), reason, eventID)
	}
	if err != nil {
		return &StoreError{Op: "retry", Err: err}
	}
	return nil
}

// ReclaimStale returns claims older than staleAfter to pending — crash
// recovery for a node that died after claiming and before an outcome.
// Each reclaim consumes an attempt (a node that dies on the same event
// every time must not loop forever), and at maxAttempts the event is
// dead. Returns the number of rows touched.
func (s *Store) ReclaimStale(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int64, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE state_events
		    SET status = CASE WHEN attempts + 1 >= ? THEN 'dead' ELSE 'pending' END,
		        attempts = attempts + 1, next_attempt_at = ?, last_error = 'claim reclaimed: stale',
		        claimed_by = NULL, claimed_at = NULL
		  WHERE status = 'claimed' AND claimed_at < ?`),
		maxAttempts, formatAt(now), formatAt(now.Add(-staleAfter)))
	if err != nil {
		return 0, &StoreError{Op: "reclaim stale", Err: err}
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Purge deletes terminal events (done, skipped, dead) created before
// retention ago. Pending and claimed rows are never purged.
func (s *Store) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.rb(
		`DELETE FROM state_events
		  WHERE status IN ('done', 'skipped', 'dead') AND created_at < ?`),
		formatAt(s.now().Add(-retention)))
	if err != nil {
		return 0, &StoreError{Op: "purge", Err: err}
	}
	n, _ := res.RowsAffected()
	return n, nil
}
