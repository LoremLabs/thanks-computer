package workspace

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Lifecycle states recorded in workspaces.status.
const (
	StatusCreated   = "created"
	StatusRunning   = "running"
	StatusWarm      = "warm"
	StatusCold      = "cold"
	StatusDestroyed = "destroyed"
)

// Row is one workspace identity: (tenant, stack, name) → the provider's
// reference, plus the lifecycle bookkeeping the chassis keeps about it.
type Row struct {
	ID            string // sha256(tenant\0stack\0name) hex — see ID
	Tenant        string // slug
	Stack         string
	Name          string
	Provider      string
	ProviderRef   string
	Runtime       string
	Network       string
	Status        string
	RunID         string
	CheckpointRef string
	CreatedAt     time.Time
	LastUsedAt    time.Time
	DestroyedAt   *time.Time
}

// ID is the deterministic primary key for a workspace identity, so every
// node derives the same row without a lookup.
func ID(tenant, stack, name string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + stack + "\x00" + name))
	return hex.EncodeToString(h[:])
}

// Store is the thin façade over the workspaces table (part of the shared
// RUNTIME DB, alongside tenant_sources — identity is runtime state that must
// live where the authoritative writes go). It carries the dialect for
// `?`→`$n` rebinding and a clock seam for tests, mirroring source.Store.
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

// EnsureSchema creates the workspaces table + indexes if absent. Prod gets
// the table from the runtime migration (0026 / overlay 0029); this exists so
// tests and the overlay reaper's own connection can stand it up without the
// migration runner. The DDL is the portable subset the migration uses.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workspaces (
			workspace_id   TEXT PRIMARY KEY,
			tenant         TEXT NOT NULL,
			stack          TEXT NOT NULL,
			name           TEXT NOT NULL,
			provider       TEXT NOT NULL,
			provider_ref   TEXT NOT NULL,
			runtime        TEXT,
			network        TEXT NOT NULL DEFAULT 'public',
			status         TEXT NOT NULL,
			run_id         TEXT,
			checkpoint_ref TEXT,
			created_at     TEXT NOT NULL,
			last_used_at   TEXT NOT NULL,
			destroyed_at   TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS workspaces_identity_idx
			ON workspaces (tenant, stack, name)`,
		`CREATE INDEX IF NOT EXISTS workspaces_last_used_idx
			ON workspaces (status, last_used_at)`,
	}
	for _, q := range append(stmts, leaseDDL...) {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("workspace: ensure schema: %w", err)
		}
	}
	return nil
}

const rowCols = `workspace_id, tenant, stack, name, provider, provider_ref, runtime, network,
	status, run_id, checkpoint_ref, created_at, last_used_at, destroyed_at`

func scanRow(sc interface{ Scan(...any) error }) (*Row, error) {
	var r Row
	var runtime, runID, cp, destroyed sql.NullString
	var created, lastUsed string
	if err := sc.Scan(&r.ID, &r.Tenant, &r.Stack, &r.Name, &r.Provider, &r.ProviderRef, &runtime, &r.Network,
		&r.Status, &runID, &cp, &created, &lastUsed, &destroyed); err != nil {
		return nil, err
	}
	r.Runtime, r.RunID, r.CheckpointRef = runtime.String, runID.String, cp.String
	r.CreatedAt, _ = time.Parse(time.RFC3339, created)
	r.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed)
	if destroyed.Valid && destroyed.String != "" {
		if t, err := time.Parse(time.RFC3339, destroyed.String); err == nil {
			r.DestroyedAt = &t
		}
	}
	return &r, nil
}

// Get returns the row for (tenant, stack, name), or (nil, nil) when there is
// none.
func (s *Store) Get(ctx context.Context, tenant, stack, name string) (*Row, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+rowCols+` FROM workspaces WHERE tenant = ? AND stack = ? AND name = ?`),
		tenant, stack, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: get: %w", err)
	}
	return r, nil
}

// Upsert records a (re)created workspace: INSERT, or on the identity key
// UPDATE the provider columns, reset status, and clear destroyed_at (a
// recreated workspace reuses its row). checkpoint_ref and run_id are left
// alone by an upsert of the same provider; a recreate after destroy starts
// with the provider's fresh environment, so the caller passes empty values
// and they are written.
func (s *Store) Upsert(ctx context.Context, r Row) error {
	if r.Tenant == "" || r.Stack == "" || r.Name == "" || r.Provider == "" || r.ProviderRef == "" {
		return errors.New("workspace: upsert needs tenant, stack, name, provider, provider_ref")
	}
	if r.Status == "" {
		r.Status = StatusCreated
	}
	if r.Network == "" {
		r.Network = "public"
	}
	now := s.now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	if r.LastUsedAt.IsZero() {
		r.LastUsedAt = now
	}
	id := ID(r.Tenant, r.Stack, r.Name)
	_, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO workspaces
			(workspace_id, tenant, stack, name, provider, provider_ref, runtime, network,
			 status, run_id, checkpoint_ref, created_at, last_used_at, destroyed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (tenant, stack, name) DO UPDATE SET
			provider       = excluded.provider,
			provider_ref   = excluded.provider_ref,
			runtime        = excluded.runtime,
			network        = excluded.network,
			status         = excluded.status,
			run_id         = excluded.run_id,
			checkpoint_ref = excluded.checkpoint_ref,
			last_used_at   = excluded.last_used_at,
			destroyed_at   = NULL`),
		id, r.Tenant, r.Stack, r.Name, r.Provider, r.ProviderRef, nullIfEmpty(r.Runtime), r.Network,
		r.Status, nullIfEmpty(r.RunID), nullIfEmpty(r.CheckpointRef),
		r.CreatedAt.UTC().Format(time.RFC3339), r.LastUsedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("workspace: upsert: %w", err)
	}
	return nil
}

// Touch records an exec: last_used_at, the run id it ran under, and the
// status the provider reported (running when unknown).
func (s *Store) Touch(ctx context.Context, id string, at time.Time, runID, status string) error {
	if status == "" {
		status = StatusRunning
	}
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE workspaces SET last_used_at = ?, run_id = ?, status = ? WHERE workspace_id = ?`),
		at.UTC().Format(time.RFC3339), nullIfEmpty(runID), status, id)
	if err != nil {
		return fmt.Errorf("workspace: touch: %w", err)
	}
	return nil
}

// SetCheckpoint records the provider's id for the latest checkpoint.
func (s *Store) SetCheckpoint(ctx context.Context, id, ref string) error {
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE workspaces SET checkpoint_ref = ? WHERE workspace_id = ?`), nullIfEmpty(ref), id)
	if err != nil {
		return fmt.Errorf("workspace: set checkpoint: %w", err)
	}
	return nil
}

// MarkDestroyed flips the row to destroyed at `at`; the row is kept so the
// identity's history survives and a later exec recreates in place.
func (s *Store) MarkDestroyed(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE workspaces SET status = ?, destroyed_at = ?, run_id = NULL WHERE workspace_id = ?`),
		StatusDestroyed, at.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("workspace: mark destroyed: %w", err)
	}
	return nil
}

// ListIdle returns live rows (not destroyed) whose last use is before
// `before`, oldest first — the reaper's scan.
func (s *Store) ListIdle(ctx context.Context, before time.Time, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT `+rowCols+` FROM workspaces
		  WHERE status <> ? AND last_used_at < ?
		  ORDER BY last_used_at ASC LIMIT ?`),
		StatusDestroyed, before.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, fmt.Errorf("workspace: list idle: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

// List returns rows, newest use first; tenant "" lists every tenant.
func (s *Store) List(ctx context.Context, tenant string, includeDestroyed bool, limit int) ([]Row, error) {
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT ` + rowCols + ` FROM workspaces WHERE 1 = 1`
	var args []any
	if tenant != "" {
		q += ` AND tenant = ?`
		args = append(args, tenant)
	}
	if !includeDestroyed {
		q += ` AND status <> ?`
		args = append(args, StatusDestroyed)
	}
	q += ` ORDER BY last_used_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.rb(q), args...)
	if err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	defer rows.Close()
	return collect(rows)
}

func collect(rows *sql.Rows) ([]Row, error) {
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("workspace: scan: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
