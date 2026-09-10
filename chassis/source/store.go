package source

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Store is the thin façade over the tenant_sources table (part of the shared
// RUNTIME DB, alongside tenant_secrets — not a separate backend, because a
// source's cursor and claim are runtime state that must live where the
// authoritative writes go). It carries the dialect for `?`→`$n` rebinding and
// the Postgres SKIP LOCKED claim, and a clock seam for tests, mirroring
// scheduled.Store and secrets.Store.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
}

// NewStore builds a Store over the opened runtime DB and its dialect. A nil
// dialect defaults to SQLite (the in-tree default).
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// SourceID is the deterministic primary key for a declared source: a hash of
// its natural key so a redeploy targets the same row (and its cursor) rather
// than minting a new one. srcseed and the store agree on it via this helper.
func SourceID(tenant, stack, pack, declaredID string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + stack + "\x00" + pack + "\x00" + declaredID))
	return "src_" + hex.EncodeToString(h[:16])
}

// Declared is the config-side of a source row — the columns srcseed owns.
// The runtime columns (cursor, claim, next_poll_at) are never in here, so an
// upsert of a Declared can never rewind a live cursor.
type Declared struct {
	Tenant       string // slug
	Stack        string
	Pack         string
	DeclaredID   string
	Kind         string
	Config       json.RawMessage // the pack line, verbatim
	Enabled      bool
	EverySeconds int
	Version      int64
}

// Claimed is a source this node won at ClaimDue: everything the poller needs
// to open the connection and route the fired runs.
type Claimed struct {
	SourceID     string
	Tenant       string // slug
	Stack        string
	Pack         string
	DeclaredID   string
	Kind         string
	Config       json.RawMessage
	Cursor       Cursor // nil when never polled
	EverySeconds int
}

// EnsureSchema creates the tenant_sources table + indexes if absent. Prod
// gets the table from the runtime migration (0025); this exists so tests (and
// a belt on first boot) can stand the table up without the migration runner.
// The DDL is the portable subset the migration uses.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tenant_sources (
			source_id        TEXT PRIMARY KEY,
			tenant           TEXT NOT NULL,
			stack            TEXT NOT NULL,
			pack             TEXT NOT NULL,
			declared_id      TEXT NOT NULL,
			kind             TEXT NOT NULL,
			config           TEXT NOT NULL,
			enabled          INTEGER NOT NULL DEFAULT 1,
			every_seconds    INTEGER NOT NULL DEFAULT 300,
			declared_version INTEGER NOT NULL DEFAULT 0,
			retired_at       TEXT,
			cursor           TEXT,
			status           TEXT NOT NULL DEFAULT 'idle',
			claimed_by       TEXT,
			claimed_at       TEXT,
			next_poll_at     TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z',
			last_poll_at     TEXT,
			last_error       TEXT,
			attempts         INTEGER NOT NULL DEFAULT 0,
			created_at       TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS tenant_sources_declared_uq
			ON tenant_sources (tenant, stack, pack, declared_id)`,
		`CREATE INDEX IF NOT EXISTS tenant_sources_due_idx
			ON tenant_sources (next_poll_at)
			WHERE enabled = 1 AND retired_at IS NULL AND status = 'idle'`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("source: ensure schema: %w", err)
		}
	}
	return nil
}

// Upsert writes a declared source: it INSERTs the row (due immediately) or,
// if one already exists for (tenant, stack, pack, declared_id), UPDATEs ONLY
// the declared columns and clears retired_at (re-adding a source un-retires
// it and resumes from its stored cursor). The runtime columns — cursor,
// status, claim, next_poll_at — are deliberately absent from the UPDATE SET,
// so a redeploy never rewinds a live cursor or steals a claim.
func (s *Store) Upsert(ctx context.Context, d Declared) error {
	if d.Tenant == "" || d.Stack == "" || d.Pack == "" || d.DeclaredID == "" {
		return errors.New("source: upsert needs tenant, stack, pack, declared_id")
	}
	if d.Kind == "" {
		return errors.New("source: upsert needs a kind")
	}
	if len(d.Config) == 0 || !json.Valid(d.Config) {
		return errors.New("source: upsert config is not valid JSON")
	}
	if d.EverySeconds <= 0 {
		d.EverySeconds = 300
	}
	id := SourceID(d.Tenant, d.Stack, d.Pack, d.DeclaredID)
	now := s.now().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO tenant_sources
			(source_id, tenant, stack, pack, declared_id, kind, config,
			 enabled, every_seconds, declared_version, created_at, next_poll_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tenant, stack, pack, declared_id) DO UPDATE SET
			kind             = excluded.kind,
			config           = excluded.config,
			enabled          = excluded.enabled,
			every_seconds    = excluded.every_seconds,
			declared_version = excluded.declared_version,
			retired_at       = NULL`),
		id, d.Tenant, d.Stack, d.Pack, d.DeclaredID, d.Kind, string(d.Config),
		boolToInt(d.Enabled), d.EverySeconds, d.Version, now, now)
	if err != nil {
		return fmt.Errorf("source: upsert: %w", err)
	}
	return nil
}

// RetireMissing soft-retires the rows of one (tenant, stack, pack) whose
// declared_id is NOT in keep — the sources a redeploy removed from a pack
// that is still present. Soft (retired_at set, row kept) so re-adding the
// source resumes from its cursor instead of re-reading the whole mailbox. An
// empty keep retires every row of that pack. Returns the retired count.
func (s *Store) RetireMissing(ctx context.Context, tenant, stack, pack string, keep []string) (int64, error) {
	now := s.now().Format(time.RFC3339)
	q := `UPDATE tenant_sources SET retired_at = ?
	       WHERE tenant = ? AND stack = ? AND pack = ? AND retired_at IS NULL`
	args := []any{now, tenant, stack, pack}
	if len(keep) > 0 {
		ph := make([]string, len(keep))
		for i, id := range keep {
			ph[i] = "?"
			args = append(args, id)
		}
		q += " AND declared_id NOT IN (" + strings.Join(ph, ",") + ")"
	}
	res, err := s.db.ExecContext(ctx, s.rb(q), args...)
	if err != nil {
		return 0, fmt.Errorf("source: retire missing: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClaimDue claims up to limit due sources (enabled, live, unclaimed, and past
// next_poll_at) in ONE statement and returns the rows this call won — the
// same claim-is-the-coordination shape as scheduled.ClaimDue. On Postgres the
// due subselect takes FOR UPDATE SKIP LOCKED so concurrent pollers partition
// the due set; on SQLite the single statement is atomic under the one writer.
func (s *Store) ClaimDue(ctx context.Context, node string, limit int) ([]Claimed, error) {
	if limit <= 0 {
		limit = 50
	}
	now := s.now().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, s.rb(
		`UPDATE tenant_sources
		    SET status = 'claimed', claimed_by = ?, claimed_at = ?, attempts = attempts + 1
		  WHERE source_id IN (SELECT source_id FROM tenant_sources
		                       WHERE enabled = 1 AND retired_at IS NULL
		                         AND status = 'idle' AND next_poll_at <= ?
		                       ORDER BY next_poll_at
		                       LIMIT ?`+s.dialect.SkipLockedClause()+`)
		  RETURNING source_id, tenant, stack, pack, declared_id, kind, config, cursor, every_seconds`),
		node, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("source: claim due: %w", err)
	}
	defer rows.Close()
	var won []Claimed
	for rows.Next() {
		var c Claimed
		var cfg string
		var cur sql.NullString
		if err := rows.Scan(&c.SourceID, &c.Tenant, &c.Stack, &c.Pack, &c.DeclaredID,
			&c.Kind, &cfg, &cur, &c.EverySeconds); err != nil {
			return won, fmt.Errorf("source: scan claimed: %w", err)
		}
		c.Config = json.RawMessage(cfg)
		if cur.Valid && cur.String != "" {
			c.Cursor = Cursor(cur.String)
		}
		won = append(won, c)
	}
	if err := rows.Err(); err != nil {
		return won, fmt.Errorf("source: claimed rows: %w", err)
	}
	return won, nil
}

// Release ends a claim: it writes the advanced cursor (nil leaves it), records
// the outcome (last_error empty on success), stamps last_poll_at, schedules
// the next poll every_seconds out, and returns the row to 'idle'. Called once
// per claimed source after its poll pass — the mirror of ClaimDue.
func (s *Store) Release(ctx context.Context, sourceID string, cursor Cursor, everySeconds int, pollErr error) error {
	if everySeconds <= 0 {
		everySeconds = 300
	}
	now := s.now()
	next := now.Add(time.Duration(everySeconds) * time.Second).Format(time.RFC3339)
	var lastErr any
	if pollErr != nil {
		lastErr = truncErr(pollErr.Error())
	}
	// cursor: NULL keeps the stored value; a non-nil value overwrites it.
	var cur any
	setCursor := ""
	if cursor != nil {
		cur = string(cursor)
		setCursor = "cursor = ?, "
	}
	args := []any{}
	if cursor != nil {
		args = append(args, cur)
	}
	args = append(args, lastErr, now.Format(time.RFC3339), next, sourceID)
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE tenant_sources
		    SET `+setCursor+`status = 'idle', claimed_by = NULL, claimed_at = NULL,
		        last_error = ?, last_poll_at = ?, next_poll_at = ?
		  WHERE source_id = ?`), args...)
	if err != nil {
		return fmt.Errorf("source: release: %w", err)
	}
	return nil
}

// ReclaimStale returns sources stuck 'claimed' past staleAfter to 'idle' so
// another pass retries them — crash recovery for a node that died mid-poll.
// It leaves next_poll_at alone (it was already <= now), so the row is due at
// once. Returns the reset count.
func (s *Store) ReclaimStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	cutoff := s.now().Add(-staleAfter).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE tenant_sources
		    SET status = 'idle', claimed_by = NULL, claimed_at = NULL
		  WHERE status = 'claimed' AND claimed_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("source: reclaim stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Status is one row's operator-visible state, for `txco source status`. It
// carries no secret — `config` is metadata (the secret is a name) and is
// omitted here; the value never existed in this table.
type Status struct {
	SourceID   string `json:"source_id"`
	Stack      string `json:"stack"`
	Pack       string `json:"pack"`
	DeclaredID string `json:"declared_id"`
	Kind       string `json:"kind"`
	Enabled    bool   `json:"enabled"`
	Retired    bool   `json:"retired"`
	Status     string `json:"status"`
	ClaimedBy  string `json:"claimed_by,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	NextPollAt string `json:"next_poll_at,omitempty"`
	LastPollAt string `json:"last_poll_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	Attempts   int    `json:"attempts"`
}

// List returns every source of one tenant (including retired), newest claim
// first, for the read-only status CLI.
func (s *Store) List(ctx context.Context, tenant string) ([]Status, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT source_id, stack, pack, declared_id, kind, enabled, retired_at,
		        status, claimed_by, cursor, next_poll_at, last_poll_at, last_error, attempts
		   FROM tenant_sources WHERE tenant = ?
		  ORDER BY stack, pack, declared_id`), tenant)
	if err != nil {
		return nil, fmt.Errorf("source: list: %w", err)
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var st Status
		var enabled int
		var retired, claimedBy, cur, nextP, lastP, lastErr sql.NullString
		if err := rows.Scan(&st.SourceID, &st.Stack, &st.Pack, &st.DeclaredID, &st.Kind,
			&enabled, &retired, &st.Status, &claimedBy, &cur, &nextP, &lastP, &lastErr, &st.Attempts); err != nil {
			return out, fmt.Errorf("source: scan status: %w", err)
		}
		st.Enabled = enabled != 0
		st.Retired = retired.Valid
		st.ClaimedBy = claimedBy.String
		st.Cursor = cur.String
		st.NextPollAt = nextP.String
		st.LastPollAt = lastP.String
		st.LastError = lastErr.String
		out = append(out, st)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncErr bounds a stored error message so a chatty upstream error can't
// bloat the row.
func truncErr(s string) string {
	const max = 1024
	if len(s) > max {
		return s[:max]
	}
	return s
}
