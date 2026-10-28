// Package gitstore is the index behind the chassis `git` personality and
// the txco://repo/* ops: which repositories a tenant has, which objects
// each holds (an object id → the content-addressed store's key, with the
// object's type and size), and where its refs point. The bytes are not
// here: a git object's content lives in the chassis's content-addressed
// store (chassis/filecas) under the sha256 of that content, so a blob's
// key is the file's own hash and the store deduplicates across everything
// else the chassis keeps. This index is the tenant fence over that shared
// store: an object is served only when the repository's row names it,
// never by probing the store on a client's behalf.
//
// Refs move by compare-and-swap, all of a push's refs or none, in one
// write transaction; every move leaves a log row saying who moved what
// from where to where (the audit the design asks for: the principal and
// credential of a push, the stack of an op). There is no garbage
// collection: objects a ref no longer reaches stay indexed, as the blob
// plane's do (docs/advanced/blobs.md).
//
// Storage is dialect-aware (registry.Dialect, the seam the drive, calendar
// and contacts indexes use); the bundled backend is a SQLite file of its
// own, never the runtime DB (a runtime-DB write reloads the whole dbcache
// mirror). A fleet overlay registers a shared Postgres backend the same
// way so the head on any node serves what an op on any node wrote.
package gitstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// ZeroOID is git's "no object": the Old of a ref being created, the New of
// one being deleted.
const ZeroOID = "0000000000000000000000000000000000000000"

var (
	// ErrNotFound is a missing repository.
	ErrNotFound = errors.New("gitstore: not found")
)

// ErrRefMoved is the compare-and-swap failure: the ref is not at the Old the
// writer named. Current is ZeroOID for a ref that does not exist.
type ErrRefMoved struct {
	Name    string
	Current string
}

func (e *ErrRefMoved) Error() string {
	return fmt.Sprintf("gitstore: ref %s moved: now %s", e.Name, e.Current)
}

// Quotas are a repository's own caps; zero means "the head's default".
type Quotas struct {
	MaxObjectBytes int64
	MaxPackBytes   int64
	MaxRepoBytes   int64
	MaxObjects     int64
}

// Repo is one repository's row.
type Repo struct {
	// ID is `rp_<hxid>`, minted at create.
	ID string
	// Tenant is the tenant slug (the one the hostname resolves to and the
	// ops run as).
	Tenant string
	// Name is the repository's name: the path segment a client clones
	// (`/git/<name>.git`) and the instance of its `git:<name>:*` scopes.
	Name string
	// Principal owns the repository: the identity whose credentials the
	// application issues for it. Recorded for the administrative ops; a
	// push is authorized by its credential's scopes, not by this.
	Principal string
	// CreatedBy is the stack that made the repository: the one that may
	// remove it.
	CreatedBy string
	// PolicyPush is "stack" (a push is accepted only when the tenant's
	// `_git` stack answers ok) or "open" (no lane).
	PolicyPush string
	// Head is the ref HEAD names (refs/heads/main).
	Head      string
	Quotas    Quotas
	CreatedAt time.Time
}

// Object is one indexed git object: the id, what it is, how big, and where
// its content is.
type Object struct {
	OID    string
	Type   string // blob | tree | commit | tag
	Size   int64
	SHA256 string
	// Inline holds the content itself for a small object, when a backend
	// chooses to keep it in the row (nil: the content is in the store).
	Inline []byte
}

// Ref is one ref.
type Ref struct {
	Name       string
	OID        string
	UpdatedAt  time.Time
	UpdatedBy  string
	Credential string
}

// RefUpdate is one move: Old must be the current value (ZeroOID when the
// ref must not exist), New the value to set (ZeroOID to delete).
type RefUpdate struct {
	Name string
	Old  string
	New  string
}

// Actor is who moved refs, for the log.
type Actor struct {
	Principal  string
	Credential string
	// Source is "push" or "op".
	Source string
	// Stack is the stack whose op moved the ref ("" for a push).
	Stack string
}

// RefLogEntry is one row of the audit log.
type RefLogEntry struct {
	ID         string
	Name       string
	Old        string
	New        string
	Principal  string
	Credential string
	Source     string
	Stack      string
	At         time.Time
}

// Store is the façade over the index tables.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
}

// NewStore builds a Store over the opened DB and its dialect. A nil dialect
// defaults to SQLite.
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and backends. Not for request paths.
func (s *Store) DB() *sql.DB { return s.db }

// SetClock pins the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// EnsureSchema creates the tables and indexes if absent. Portable DDL: TEXT
// ids, TEXT RFC3339 timestamps, BIGINT counters, BYTEA for the one binary
// column — one DDL serves SQLite and Postgres. Additive-only.
func (s *Store) EnsureSchema(ctx context.Context) error {
	return registry.RetrySchema(ctx, s.ensureSchema)
}

func (s *Store) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS repos (
			id               TEXT PRIMARY KEY,
			tenant           TEXT NOT NULL,
			name             TEXT NOT NULL,
			principal        TEXT NOT NULL DEFAULT '',
			created_by       TEXT NOT NULL DEFAULT '',
			policy_push      TEXT NOT NULL DEFAULT 'stack',
			head             TEXT NOT NULL DEFAULT 'refs/heads/main',
			max_object_bytes BIGINT NOT NULL DEFAULT 0,
			max_pack_bytes   BIGINT NOT NULL DEFAULT 0,
			max_repo_bytes   BIGINT NOT NULL DEFAULT 0,
			max_objects      BIGINT NOT NULL DEFAULT 0,
			created_at       TEXT NOT NULL,
			UNIQUE (tenant, name)
		)`,
		`CREATE TABLE IF NOT EXISTS repo_objects (
			repo_id    TEXT NOT NULL,
			oid        TEXT NOT NULL,
			type       TEXT NOT NULL,
			size       BIGINT NOT NULL,
			sha256     TEXT NOT NULL,
			inline     BYTEA,
			created_at TEXT NOT NULL,
			PRIMARY KEY (repo_id, oid)
		)`,
		`CREATE INDEX IF NOT EXISTS repo_objects_type_idx ON repo_objects (repo_id, type)`,
		`CREATE TABLE IF NOT EXISTS repo_refs (
			repo_id    TEXT NOT NULL,
			name       TEXT NOT NULL,
			oid        TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			updated_by TEXT NOT NULL DEFAULT '',
			credential TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (repo_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS repo_ref_log (
			id         TEXT PRIMARY KEY,
			repo_id    TEXT NOT NULL,
			name       TEXT NOT NULL,
			old_oid    TEXT NOT NULL,
			new_oid    TEXT NOT NULL,
			principal  TEXT NOT NULL DEFAULT '',
			credential TEXT NOT NULL DEFAULT '',
			source     TEXT NOT NULL DEFAULT '',
			stack      TEXT NOT NULL DEFAULT '',
			at         TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS repo_ref_log_repo_idx ON repo_ref_log (repo_id, at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("gitstore: ensure schema: %w", err)
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

func isZero(oid string) bool { return oid == "" || oid == ZeroOID }

func norm(oid string) string {
	oid = strings.ToLower(strings.TrimSpace(oid))
	if oid == "" {
		return ZeroOID
	}
	return oid
}

// ── repositories ─────────────────────────────────────────────────────────

const repoCols = `id, tenant, name, principal, created_by, policy_push, head,
	max_object_bytes, max_pack_bytes, max_repo_bytes, max_objects, created_at`

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var created string
	err := row.Scan(&r.ID, &r.Tenant, &r.Name, &r.Principal, &r.CreatedBy, &r.PolicyPush, &r.Head,
		&r.Quotas.MaxObjectBytes, &r.Quotas.MaxPackBytes, &r.Quotas.MaxRepoBytes, &r.Quotas.MaxObjects, &created)
	r.CreatedAt = parseTime(created)
	return r, err
}

// CreateRepo makes the repository, or returns the existing one of that
// tenant and name (created = false): the create is idempotent so an
// application may "ensure" a repository. The id is minted here; Head
// defaults to refs/heads/main, PolicyPush to "stack".
func (s *Store) CreateRepo(ctx context.Context, r Repo) (Repo, bool, error) {
	if existing, found, err := s.GetRepo(ctx, r.Tenant, r.Name); err != nil {
		return Repo{}, false, err
	} else if found {
		return existing, false, nil
	}
	if r.ID == "" {
		r.ID = "rp_" + hxid.New().String()
	}
	if r.Head == "" {
		r.Head = "refs/heads/main"
	}
	if r.PolicyPush == "" {
		r.PolicyPush = "stack"
	}
	r.CreatedAt = s.now()
	_, err := s.db.ExecContext(ctx, s.rb(`INSERT INTO repos (`+repoCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.Tenant, r.Name, r.Principal, r.CreatedBy, r.PolicyPush, r.Head,
		r.Quotas.MaxObjectBytes, r.Quotas.MaxPackBytes, r.Quotas.MaxRepoBytes, r.Quotas.MaxObjects, fmtTime(r.CreatedAt))
	if err != nil {
		if s.dialect.IsUniqueViolationGeneric(err) {
			// Lost a race with another creator: theirs stands.
			if existing, found, gerr := s.GetRepo(ctx, r.Tenant, r.Name); gerr == nil && found {
				return existing, false, nil
			}
		}
		return Repo{}, false, fmt.Errorf("gitstore: create repo: %w", err)
	}
	return r, true, nil
}

// GetRepo finds a tenant's repository by name.
func (s *Store) GetRepo(ctx context.Context, tenant, name string) (Repo, bool, error) {
	r, err := scanRepo(s.db.QueryRowContext(ctx, s.rb(`SELECT `+repoCols+` FROM repos WHERE tenant = ? AND name = ?`), tenant, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, false, nil
	}
	if err != nil {
		return Repo{}, false, fmt.Errorf("gitstore: get repo: %w", err)
	}
	return r, true, nil
}

// GetRepoByID finds a repository by id.
func (s *Store) GetRepoByID(ctx context.Context, id string) (Repo, bool, error) {
	r, err := scanRepo(s.db.QueryRowContext(ctx, s.rb(`SELECT `+repoCols+` FROM repos WHERE id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, false, nil
	}
	if err != nil {
		return Repo{}, false, fmt.Errorf("gitstore: get repo: %w", err)
	}
	return r, true, nil
}

// ListRepos lists a tenant's repositories by name.
func (s *Store) ListRepos(ctx context.Context, tenant string) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT `+repoCols+` FROM repos WHERE tenant = ? ORDER BY name`), tenant)
	if err != nil {
		return nil, fmt.Errorf("gitstore: list repos: %w", err)
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("gitstore: list repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemoveRepo deletes a repository and its index rows (refs, objects, log).
// The objects' content stays in the content-addressed store. ErrNotFound
// when there is no such repository.
func (s *Store) RemoveRepo(ctx context.Context, id string) error {
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return fmt.Errorf("gitstore: remove repo: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.rb(`DELETE FROM repos WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("gitstore: remove repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{
		`DELETE FROM repo_refs WHERE repo_id = ?`,
		`DELETE FROM repo_objects WHERE repo_id = ?`,
		`DELETE FROM repo_ref_log WHERE repo_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, s.rb(q), id); err != nil {
			return fmt.Errorf("gitstore: remove repo: %w", err)
		}
	}
	return tx.Commit()
}

// ── objects ──────────────────────────────────────────────────────────────

// GetObject finds one object of the repository.
func (s *Store) GetObject(ctx context.Context, repoID, oid string) (Object, bool, error) {
	var o Object
	err := s.db.QueryRowContext(ctx, s.rb(`SELECT oid, type, size, sha256, inline FROM repo_objects WHERE repo_id = ? AND oid = ?`),
		repoID, norm(oid)).Scan(&o.OID, &o.Type, &o.Size, &o.SHA256, &o.Inline)
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, fmt.Errorf("gitstore: get object: %w", err)
	}
	return o, true, nil
}

// PutObjects indexes objects; one already indexed is left alone (the same
// id is the same content).
func (s *Store) PutObjects(ctx context.Context, repoID string, objs []Object) error {
	if len(objs) == 0 {
		return nil
	}
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return fmt.Errorf("gitstore: put objects: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := fmtTime(s.now())
	q := s.rb(`INSERT INTO repo_objects (repo_id, oid, type, size, sha256, inline, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (repo_id, oid) DO NOTHING`)
	for _, o := range objs {
		if _, err := tx.ExecContext(ctx, q, repoID, norm(o.OID), o.Type, o.Size, o.SHA256, o.Inline, now); err != nil {
			return fmt.Errorf("gitstore: put object %s: %w", o.OID, err)
		}
	}
	return tx.Commit()
}

// ListObjects lists the repository's objects, of one type or all ("").
// Metadata only (no inline content).
func (s *Store) ListObjects(ctx context.Context, repoID, typ string) ([]Object, error) {
	q := `SELECT oid, type, size, sha256 FROM repo_objects WHERE repo_id = ?`
	args := []any{repoID}
	if typ != "" {
		q += ` AND type = ?`
		args = append(args, typ)
	}
	rows, err := s.db.QueryContext(ctx, s.rb(q+` ORDER BY oid`), args...)
	if err != nil {
		return nil, fmt.Errorf("gitstore: list objects: %w", err)
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.OID, &o.Type, &o.Size, &o.SHA256); err != nil {
			return nil, fmt.Errorf("gitstore: list objects: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ObjectStats reports how many objects the repository indexes and their
// total size (the repository's size for the quota).
func (s *Store) ObjectStats(ctx context.Context, repoID string) (count, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, s.rb(`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM repo_objects WHERE repo_id = ?`), repoID).Scan(&count, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("gitstore: object stats: %w", err)
	}
	return count, bytes, nil
}

// ── refs ─────────────────────────────────────────────────────────────────

// ListRefs lists the repository's refs by name.
func (s *Store) ListRefs(ctx context.Context, repoID string) ([]Ref, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT name, oid, updated_at, updated_by, credential FROM repo_refs WHERE repo_id = ? ORDER BY name`), repoID)
	if err != nil {
		return nil, fmt.Errorf("gitstore: list refs: %w", err)
	}
	defer rows.Close()
	var out []Ref
	for rows.Next() {
		var r Ref
		var at string
		if err := rows.Scan(&r.Name, &r.OID, &at, &r.UpdatedBy, &r.Credential); err != nil {
			return nil, fmt.Errorf("gitstore: list refs: %w", err)
		}
		r.UpdatedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRef finds one ref.
func (s *Store) GetRef(ctx context.Context, repoID, name string) (Ref, bool, error) {
	var r Ref
	var at string
	err := s.db.QueryRowContext(ctx, s.rb(`SELECT name, oid, updated_at, updated_by, credential FROM repo_refs WHERE repo_id = ? AND name = ?`),
		repoID, name).Scan(&r.Name, &r.OID, &at, &r.UpdatedBy, &r.Credential)
	if errors.Is(err, sql.ErrNoRows) {
		return Ref{}, false, nil
	}
	if err != nil {
		return Ref{}, false, fmt.Errorf("gitstore: get ref: %w", err)
	}
	r.UpdatedAt = parseTime(at)
	return r, true, nil
}

// CompareAndSetRefs moves refs, all or none: every update's Old must equal
// the ref's current value (ZeroOID for one that does not exist), or the
// first mismatch is returned as *ErrRefMoved and nothing changes. A ZeroOID
// New deletes the ref. Each move leaves a log row naming the actor.
func (s *Store) CompareAndSetRefs(ctx context.Context, repoID string, updates []RefUpdate, by Actor) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return fmt.Errorf("gitstore: update refs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Check every ref under the write lock before touching any.
	for _, u := range updates {
		var cur string
		err := tx.QueryRowContext(ctx, s.rb(`SELECT oid FROM repo_refs WHERE repo_id = ? AND name = ?`+s.dialect.LockClause()),
			repoID, u.Name).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			cur = ZeroOID
		case err != nil:
			return fmt.Errorf("gitstore: update refs: %w", err)
		}
		if norm(u.Old) != norm(cur) {
			return &ErrRefMoved{Name: u.Name, Current: norm(cur)}
		}
	}
	now := fmtTime(s.now())
	for _, u := range updates {
		old, nw := norm(u.Old), norm(u.New)
		var res sql.Result
		switch {
		case isZero(nw):
			res, err = tx.ExecContext(ctx, s.rb(`DELETE FROM repo_refs WHERE repo_id = ? AND name = ? AND oid = ?`), repoID, u.Name, old)
		case isZero(old):
			res, err = tx.ExecContext(ctx, s.rb(`INSERT INTO repo_refs (repo_id, name, oid, updated_at, updated_by, credential) VALUES (?, ?, ?, ?, ?, ?)`),
				repoID, u.Name, nw, now, by.Principal, by.Credential)
			if err != nil && s.dialect.IsUniqueViolationGeneric(err) {
				// Another writer created it between our check and our insert
				// (an engine without the row lock): theirs stands.
				return &ErrRefMoved{Name: u.Name, Current: "?"}
			}
		default:
			res, err = tx.ExecContext(ctx, s.rb(`UPDATE repo_refs SET oid = ?, updated_at = ?, updated_by = ?, credential = ? WHERE repo_id = ? AND name = ? AND oid = ?`),
				nw, now, by.Principal, by.Credential, repoID, u.Name, old)
		}
		if err != nil {
			return fmt.Errorf("gitstore: update ref %s: %w", u.Name, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return &ErrRefMoved{Name: u.Name, Current: "?"}
		}
		if _, err := tx.ExecContext(ctx, s.rb(`INSERT INTO repo_ref_log (id, repo_id, name, old_oid, new_oid, principal, credential, source, stack, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"rl_"+hxid.New().String(), repoID, u.Name, old, nw, by.Principal, by.Credential, by.Source, by.Stack, now); err != nil {
			return fmt.Errorf("gitstore: log ref %s: %w", u.Name, err)
		}
	}
	return tx.Commit()
}

// RefLog lists the newest moves of a repository's refs, newest first.
func (s *Store) RefLog(ctx context.Context, repoID string, limit int) ([]RefLogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT id, name, old_oid, new_oid, principal, credential, source, stack, at
		FROM repo_ref_log WHERE repo_id = ? ORDER BY at DESC, id DESC LIMIT ?`), repoID, limit)
	if err != nil {
		return nil, fmt.Errorf("gitstore: ref log: %w", err)
	}
	defer rows.Close()
	var out []RefLogEntry
	for rows.Next() {
		var e RefLogEntry
		var at string
		if err := rows.Scan(&e.ID, &e.Name, &e.Old, &e.New, &e.Principal, &e.Credential, &e.Source, &e.Stack, &at); err != nil {
			return nil, fmt.Errorf("gitstore: ref log: %w", err)
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
