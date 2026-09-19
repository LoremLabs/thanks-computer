// Package drive is the mutable document store behind the chassis `webdav`
// personality and the txco://drive/* ops: per-tenant collections of files
// and directories whose bytes live in an ordinary object store and whose
// names, versions and hierarchy live in a SQL index.
//
// It is the third storage plane beside the stack's FILES/ (immutable,
// content-addressed, served) and the blob layer (named content over the
// same CAS). A drive resource is MUTABLE: PUT replaces, MOVE renames,
// DELETE deletes. What stays fixed is the resource id — `dr_<hxid>`, minted
// once when the resource is created and carried through every rename and
// rewrite. The path is where the resource is; the etag (sha256 of the
// current bytes) is which version it is. A consumer that keys on the
// resource id survives a rename; one that keys on the etag sees every
// content change.
//
// Bytes first, metadata second (the house rule): a put streams the body to
// a fresh object key, then opens the index transaction; an object without
// a live row is an orphan the sweeper reclaims, never a row without bytes.
// Deletes leave a tombstone (deleted_at) with a bumped modseq so a `list
// since=<modseq>` can report removals; the partial unique index on
// (collection_id, path) WHERE deleted_at IS NULL frees the path at once, and
// the sweeper hard-deletes tombstones past a retention window.
//
// Every mutation advances the collection's sync_token in the same
// transaction and reports itself to the configured MutationSink; with a
// transactional sink (the scheduled store on the same database) the event
// row commits with the mutation.
//
// Storage is dialect-aware (registry.Dialect, the seam auth, scheduled,
// calendar and contacts use); the bundled index backend is a SQLite file of
// its own, never the runtime DB.
package drive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Account statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Resource kinds.
const (
	KindFile = "file"
	KindDir  = "dir"
)

var (
	// ErrNotFound is a missing collection, account or resource.
	ErrNotFound = errors.New("drive: not found")
	// ErrExists is a create that found a live resource at the path (mkdir,
	// or a move/copy without overwrite).
	ErrExists = errors.New("drive: resource exists")
	// ErrNoParent is a write whose parent directory does not exist.
	ErrNoParent = errors.New("drive: parent directory does not exist")
	// ErrNotDirectory is a directory operation on a file.
	ErrNotDirectory = errors.New("drive: not a directory")
	// ErrIsDirectory is a file operation on a directory.
	ErrIsDirectory = errors.New("drive: is a directory")
	// ErrPrecondition is a failed If-Match / If-None-Match.
	ErrPrecondition = errors.New("drive: precondition failed")
	// ErrQuota is a write that would exceed the collection's byte or
	// resource budget.
	ErrQuota = errors.New("drive: collection quota exceeded")
	// ErrTooLarge is a file over the per-file byte cap.
	ErrTooLarge = errors.New("drive: file too large")
	// ErrSizeMismatch is a put whose body was shorter or longer than its
	// declared size.
	ErrSizeMismatch = errors.New("drive: body size does not match declared size")
	// ErrBadPath is a path that fails NormalizePath, or the root where a
	// resource path is required.
	ErrBadPath = errors.New("drive: invalid path")
	// ErrCycle is a move/copy whose destination is the source or inside it.
	ErrCycle = errors.New("drive: destination is the source or inside it")
	// ErrUsernameTaken is returned by UpsertAccount when the username already
	// belongs to another tenant (usernames are globally unique: they are the
	// login identity).
	ErrUsernameTaken = errors.New("drive: username belongs to another tenant")
	// ErrDisabled is an operation against a disabled account.
	ErrDisabled = errors.New("drive: account disabled")
	// ErrNotEmpty is a collection delete that found live resources and was
	// not forced.
	ErrNotEmpty = errors.New("drive: collection is not empty")
)

// Limits are the write-side budgets a Store enforces. Zero means unlimited.
type Limits struct {
	// MaxFileBytes caps one file's size.
	MaxFileBytes int64
	// MaxCollectionBytes caps a collection's bytes_used.
	MaxCollectionBytes int64
	// MaxResources caps a collection's live resource_count.
	MaxResources int64
}

// Store is the façade over the index tables plus the object store that holds
// the bytes. It carries the dialect (for `?`→`$n` rebinding), a clock seam
// for tests, the optional mutation sink and the write limits.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
	objects ObjectStore
	sink    MutationSink
	limits  Limits
	// onSinkErr is told when a non-transactional sink fails AFTER the
	// mutation committed (the mutation stands; only its event was lost).
	onSinkErr func(Mutation, error)
}

// NewStore builds a Store over the opened index DB, its dialect and the
// object store for bytes. A nil dialect defaults to SQLite (the in-tree
// default). The sink defaults to NopSink.
func NewStore(db *sql.DB, d registry.Dialect, objects ObjectStore) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{
		db:        db,
		dialect:   d,
		now:       func() time.Time { return time.Now().UTC() },
		objects:   objects,
		sink:      NopSink{},
		onSinkErr: func(Mutation, error) {},
	}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the underlying DB handle. The object store is the
// caller's to close.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the index handle for tests and backends. Not for request paths.
func (s *Store) DB() *sql.DB { return s.db }

// Objects exposes the object store (the sweeper and tests).
func (s *Store) Objects() ObjectStore { return s.objects }

// SetClock pins the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// SetSink installs the mutation sink. A sink that also implements
// TransactionalMutationSink is called inside the index transaction; any
// other sink is called after commit.
func (s *Store) SetSink(sink MutationSink) {
	if sink == nil {
		sink = NopSink{}
	}
	s.sink = sink
}

// SetSinkErrorHandler installs the hook told when a post-commit sink call
// fails (boot wires it to the logger).
func (s *Store) SetSinkErrorHandler(fn func(Mutation, error)) {
	if fn == nil {
		fn = func(Mutation, error) {}
	}
	s.onSinkErr = fn
}

// SetLimits installs the write budgets.
func (s *Store) SetLimits(l Limits) { s.limits = l }

// Limits reports the write budgets.
func (s *Store) Limits() Limits { return s.limits }

// EnsureSchema creates the tables + indexes if absent. Portable DDL: TEXT
// ids (hxid), TEXT RFC3339 timestamps, native partial indexes, BIGINT
// counters — one DDL serves SQLite and Postgres. Additive-only: a later
// column is added with its own IF-NOT-EXISTS-shaped statement.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS drive_collections (
			id             TEXT PRIMARY KEY,
			tenant         TEXT NOT NULL,
			name           TEXT NOT NULL,
			sync_token     BIGINT NOT NULL DEFAULT 0,
			bytes_used     BIGINT NOT NULL DEFAULT 0,
			resource_count BIGINT NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL,
			updated_at     TEXT NOT NULL,
			policy         TEXT NOT NULL DEFAULT '',
			deleted_at     TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS drive_collections_name_idx
			ON drive_collections (tenant, name) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS drive_accounts (
			tenant        TEXT NOT NULL,
			username      TEXT NOT NULL,
			status        TEXT NOT NULL DEFAULT 'active',
			collection_id TEXT NOT NULL,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL,
			PRIMARY KEY (tenant, username),
			UNIQUE (username)
		)`,
		`CREATE TABLE IF NOT EXISTS drive_resources (
			resource_id   TEXT PRIMARY KEY,
			collection_id TEXT NOT NULL,
			path          TEXT NOT NULL,
			parent_path   TEXT NOT NULL,
			depth         INTEGER NOT NULL,
			kind          TEXT NOT NULL,
			object_key    TEXT,
			size          BIGINT NOT NULL DEFAULT 0,
			content_type  TEXT NOT NULL DEFAULT '',
			etag          TEXT NOT NULL DEFAULT '',
			modseq        BIGINT NOT NULL DEFAULT 0,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL,
			deleted_at    TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS drive_resources_path_idx
			ON drive_resources (collection_id, path) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS drive_resources_parent_idx
			ON drive_resources (collection_id, parent_path) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS drive_resources_modseq_idx
			ON drive_resources (collection_id, modseq)`,
		`CREATE INDEX IF NOT EXISTS drive_resources_object_idx
			ON drive_resources (object_key)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("drive: ensure schema: %w", err)
		}
	}
	// Additive columns, for a table that predates them. CREATE TABLE IF NOT
	// EXISTS leaves an existing table alone, and the two engines disagree
	// about ALTER ... ADD COLUMN IF NOT EXISTS (Postgres has it, SQLite does
	// not), so the portable move is to PROBE and then add — the same shape
	// the boot code uses to decide whether the scheduled store shares this
	// database.
	for _, c := range []struct{ table, column, ddl string }{
		{"drive_collections", "policy", `ALTER TABLE drive_collections ADD COLUMN policy TEXT NOT NULL DEFAULT ''`},
	} {
		if _, err := s.db.ExecContext(ctx, `SELECT `+c.column+` FROM `+c.table+` WHERE 1 = 0`); err == nil {
			continue
		}
		if _, err := s.db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("drive: add %s.%s: %w", c.table, c.column, err)
		}
	}
	// The account's password moved to the identity store (chassis/authn): a
	// login is a credential that authenticates as a principal, and the
	// username only finds that principal. Drop the old column rather than
	// leave it — a column nothing reads is one someone eventually writes by
	// accident. Probe, then drop: both engines support DROP COLUMN, and
	// pw_hash is in no index or constraint. Two nodes booting together can
	// both try; the loser's error is fine once the column is gone.
	const probePwHash = `SELECT pw_hash FROM drive_accounts WHERE 1 = 0`
	if _, err := s.db.ExecContext(ctx, probePwHash); err == nil {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE drive_accounts DROP COLUMN pw_hash`); err != nil {
			if _, still := s.db.ExecContext(ctx, probePwHash); still == nil {
				return fmt.Errorf("drive: drop drive_accounts.pw_hash: %w", err)
			}
		}
	}
	return nil
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// txq is what the per-transaction helpers need from *sql.Tx (or *sql.DB for
// the read-only callers).
type txq interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type rowScanner interface{ Scan(dest ...any) error }

// lock is the dialect's row-lock suffix for a SELECT inside a write tx
// ("" on SQLite, whose BEGIN IMMEDIATE already holds the write lock).
func lock(s *Store) string {
	if c := s.dialect.LockClause(); c != "" {
		return " " + c
	}
	return ""
}
