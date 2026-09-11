package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Store is the façade over the two tables. It carries the dialect (for
// `?`→`$n` rebinding), the node limits, and a clock seam for tests,
// mirroring imap.Store.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
	limits  Limits
}

// NewStore builds a Store over the opened DB and its dialect. A nil
// dialect defaults to SQLite (the in-tree default).
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }, limits: DefaultLimits()}
}

// SetLimits installs the node's caps. A zero page size or ceiling keeps
// the default; MaxDataBytes and MaxTTL are taken as given (0 = unlimited).
func (s *Store) SetLimits(l Limits) {
	d := DefaultLimits()
	if l.DefaultReadLimit <= 0 {
		l.DefaultReadLimit = d.DefaultReadLimit
	}
	if l.MaxReadLimit <= 0 {
		l.MaxReadLimit = d.MaxReadLimit
	}
	if l.MaxDataBytes < 0 {
		l.MaxDataBytes = 0
	}
	if l.MaxTTL < 0 {
		l.MaxTTL = 0
	}
	s.limits = l
}

// Limits returns the caps in force.
func (s *Store) Limits() Limits { return s.limits }

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests and backends. Not for request paths.
func (s *Store) DB() *sql.DB { return s.db }

// EnsureSchema creates the tables + indexes if absent. Portable DDL (the
// imap store's rules): TEXT ids, TEXT fixed-width timestamps, JSON as
// TEXT, BIGINT counters, native partial indexes — one statement set
// serves SQLite and Postgres. There is no migration runner for these
// stores; changes must be additive.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notebooks (
			notebook_id TEXT PRIMARY KEY,
			tenant      TEXT NOT NULL,
			namespace   TEXT NOT NULL,
			name        TEXT NOT NULL,
			generation  BIGINT NOT NULL,
			next_seq    BIGINT NOT NULL DEFAULT 1,
			ttl_secs    BIGINT,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS notebooks_name_idx
			ON notebooks (tenant, namespace, name)`,
		`CREATE TABLE IF NOT EXISTS notebook_entries (
			notebook_id TEXT NOT NULL,
			seq         BIGINT NOT NULL,
			at          TEXT NOT NULL,
			type        TEXT NOT NULL,
			data        TEXT NOT NULL DEFAULT '{}',
			object_key  TEXT NOT NULL DEFAULT '',
			expires_at  TEXT,
			PRIMARY KEY (notebook_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS notebook_entries_at_idx
			ON notebook_entries (notebook_id, at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS notebook_entries_key_idx
			ON notebook_entries (notebook_id, object_key) WHERE object_key <> ''`,
		`CREATE INDEX IF NOT EXISTS notebook_entries_exp_idx
			ON notebook_entries (expires_at) WHERE expires_at IS NOT NULL`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("notebook: ensure schema: %w", err)
		}
	}
	return nil
}

func (s *Store) clampLimit(limit int) int {
	if limit <= 0 {
		return s.limits.DefaultReadLimit
	}
	if s.limits.MaxReadLimit > 0 && limit > s.limits.MaxReadLimit {
		return s.limits.MaxReadLimit
	}
	return limit
}

func (s *Store) clampTTL(ttl time.Duration) time.Duration {
	if ttl < 0 {
		return 0
	}
	if s.limits.MaxTTL > 0 && ttl > s.limits.MaxTTL {
		return s.limits.MaxTTL
	}
	return ttl
}

// Append records one entry and returns its position. Validation happens
// before any DB work; every DB failure comes back as a StoreError.
//
// Appends to one notebook are serialized: the transaction first locks the
// head row (LockClause — FOR UPDATE on Postgres; on SQLite the
// _txlock=immediate connection already holds the write lock), so the
// object_key check and the next_seq allocation can never interleave across
// nodes. A unique-index violation (an out-of-band writer that won the same
// key) is retried once, in a fresh transaction — Postgres aborts the
// current one on a violation.
func (s *Store) Append(ctx context.Context, req AppendReq) (AppendResult, error) {
	if err := req.Ref.Validate(); err != nil {
		return AppendResult{}, err
	}
	if err := ValidType(req.Type); err != nil {
		return AppendResult{}, err
	}
	if len(req.ObjectKey) > MaxObjectKeyBytes {
		return AppendResult{}, &TooLargeError{Field: "object_key", Max: MaxObjectKeyBytes, Got: len(req.ObjectKey)}
	}
	if !utf8.ValidString(req.ObjectKey) {
		return AppendResult{}, &InvalidArgError{Reason: "object_key is not valid UTF-8"}
	}
	data := req.Data
	if len(bytes.TrimSpace(data)) == 0 {
		data = json.RawMessage(`{}`)
	}
	// Both checks: json.Valid does not reject invalid UTF-8 inside strings,
	// and Postgres TEXT does (SQLSTATE 22021) — fail the same way on both
	// engines.
	if !utf8.Valid(data) {
		return AppendResult{}, &InvalidArgError{Reason: "data is not valid UTF-8"}
	}
	if !json.Valid(data) {
		return AppendResult{}, &InvalidArgError{Reason: "data is not valid JSON"}
	}
	if s.limits.MaxDataBytes > 0 && len(data) > s.limits.MaxDataBytes {
		return AppendResult{}, &TooLargeError{Field: "data", Max: s.limits.MaxDataBytes, Got: len(data)}
	}
	ttl := s.clampTTL(req.TTL)

	var res AppendResult
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		res, err = s.appendOnce(ctx, req.Ref, req.Type, data, req.ObjectKey, ttl)
		if err == nil || !s.dialect.IsUniqueViolationGeneric(err) {
			break
		}
	}
	if err != nil {
		return AppendResult{}, &StoreError{Op: "append", Err: err}
	}
	return res, nil
}

// appendOnce is one attempt of Append: the whole thing in one transaction.
func (s *Store) appendOnce(ctx context.Context, ref Ref, typ string, data json.RawMessage, key string, ttl time.Duration) (AppendResult, error) {
	now := s.now().UTC()
	at := FormatAt(now)
	id := ref.ID()

	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return AppendResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; also ends the duplicate early-return

	// 1. Lock the head row for the rest of the transaction.
	lockQ := s.rb(`SELECT ttl_secs FROM notebooks WHERE notebook_id = ?` + s.dialect.LockClause())
	var headTTL sql.NullInt64
	err = tx.QueryRowContext(ctx, lockQ, id).Scan(&headTTL)
	if errors.Is(err, sql.ErrNoRows) {
		// 2. First append: create the head. DO NOTHING absorbs a concurrent
		// creator without an error (a plain INSERT would abort the Postgres
		// transaction on the violation and burn the retry). The INSERT takes
		// no row lock, and a no-op'd one means someone else's row is the one
		// to lock — so re-run the locking SELECT; under READ COMMITTED it
		// sees the winner's committed row.
		gen, gerr := newGeneration()
		if gerr != nil {
			return AppendResult{}, fmt.Errorf("generation: %w", gerr)
		}
		if _, err = tx.ExecContext(ctx, s.rb(`
			INSERT INTO notebooks (notebook_id, tenant, namespace, name, generation, next_seq, ttl_secs, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, NULL, ?, ?)
			ON CONFLICT (notebook_id) DO NOTHING`),
			id, ref.Tenant, ref.Namespace, ref.Name, gen, at, at); err != nil {
			return AppendResult{}, fmt.Errorf("create head: %w", err)
		}
		err = tx.QueryRowContext(ctx, lockQ, id).Scan(&headTTL)
	}
	if err != nil {
		return AppendResult{}, fmt.Errorf("lock head: %w", err)
	}

	// 3. object_key check, under the lock. A live original is returned as
	// is; a lazily expired one is removed so "expired ⇒ gone" holds whether
	// or not the sweeper has run.
	if key != "" {
		var seq int64
		var atStr string
		var exp sql.NullString
		err := tx.QueryRowContext(ctx, s.rb(`
			SELECT seq, at, expires_at FROM notebook_entries WHERE notebook_id = ? AND object_key = ?`),
			id, key).Scan(&seq, &atStr, &exp)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return AppendResult{}, fmt.Errorf("lookup object_key: %w", err)
		case exp.Valid && exp.String <= at:
			if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM notebook_entries WHERE notebook_id = ? AND seq = ?`), id, seq); err != nil {
				return AppendResult{}, fmt.Errorf("remove expired original: %w", err)
			}
		default:
			t, _ := ParseAt(atStr)
			return AppendResult{Seq: seq, At: t, Existed: true}, nil
		}
	}

	// 4. Allocate from the head row: next_seq is stored, never MAX(seq)+1,
	// so a pruned tail never recycles a sequence number.
	var seq int64
	if err := tx.QueryRowContext(ctx, s.rb(`
		UPDATE notebooks SET next_seq = next_seq + 1, updated_at = ?
		 WHERE notebook_id = ?
		 RETURNING next_seq - 1`), at, id).Scan(&seq); err != nil {
		return AppendResult{}, fmt.Errorf("allocate seq: %w", err)
	}

	// 5. Expiry: the per-append TTL wins, else the head's default, else
	// unbounded.
	if ttl == 0 && headTTL.Valid && headTTL.Int64 > 0 {
		ttl = time.Duration(headTTL.Int64) * time.Second
	}
	var expires any // nil ⇒ SQL NULL on both drivers
	if ttl > 0 {
		expires = FormatAt(now.Add(ttl))
	}

	// 6. The entry.
	if _, err := tx.ExecContext(ctx, s.rb(`
		INSERT INTO notebook_entries (notebook_id, seq, at, type, data, object_key, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		id, seq, at, typ, string(data), key, expires); err != nil {
		if s.dialect.IsUniqueViolationGeneric(err) {
			return AppendResult{}, err // retried by Append
		}
		return AppendResult{}, fmt.Errorf("insert entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AppendResult{}, fmt.Errorf("commit: %w", err)
	}
	return AppendResult{Seq: seq, At: now}, nil
}

// Stat returns the head row. ok is false when the notebook has never been
// appended to (or was deleted).
func (s *Store) Stat(ctx context.Context, ref Ref) (Head, bool, error) {
	if err := ref.Validate(); err != nil {
		return Head{}, false, err
	}
	return s.stat(ctx, ref)
}

func (s *Store) stat(ctx context.Context, ref Ref) (Head, bool, error) {
	var h Head
	var nextSeq int64
	var ttl sql.NullInt64
	var created, updated string
	err := s.db.QueryRowContext(ctx, s.rb(`
		SELECT generation, next_seq, ttl_secs, created_at, updated_at FROM notebooks WHERE notebook_id = ?`),
		ref.ID()).Scan(&h.Generation, &nextSeq, &ttl, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Head{}, false, nil
	}
	if err != nil {
		return Head{}, false, &StoreError{Op: "stat", Err: err}
	}
	h.Name = ref.Name
	h.HighSeq = nextSeq - 1
	if ttl.Valid && ttl.Int64 > 0 {
		h.TTL = time.Duration(ttl.Int64) * time.Second
	}
	h.CreatedAt, _ = ParseAt(created)
	h.UpdatedAt, _ = ParseAt(updated)
	return h, true, nil
}

func (s *Store) validateRead(req ReadReq) error {
	if err := req.Ref.Validate(); err != nil {
		return err
	}
	if req.Tail < 0 || req.Limit < 0 || req.UpTo < 0 || req.After.Seq < 0 {
		return &InvalidArgError{Reason: "negative window"}
	}
	if !req.Since.IsZero() && !req.Until.IsZero() && !req.Since.Before(req.Until) {
		return &InvalidArgError{Reason: "since must be before until"}
	}
	if req.Type != "" {
		if err := ValidType(req.Type); err != nil {
			return err
		}
	}
	return nil
}

// Read returns one page of the selection, ascending by seq. A notebook
// that does not exist reads as empty; a cursor from an earlier generation
// is a StaleCursorError.
func (s *Store) Read(ctx context.Context, req ReadReq) (ReadResult, error) {
	if err := s.validateRead(req); err != nil {
		return ReadResult{}, err
	}
	head, ok, err := s.stat(ctx, req.Ref)
	if err != nil {
		return ReadResult{}, err
	}
	if !ok {
		return ReadResult{Entries: []Entry{}}, nil
	}
	if !req.After.IsZero() && req.After.Generation != head.Generation {
		return ReadResult{}, &StaleCursorError{}
	}
	return s.readPage(ctx, req, head.Generation)
}

// readPage is one query. Selection predicates and their args are appended
// in lockstep (Rebind numbers `?` left to right); only the ORDER BY
// direction and the LIMIT differ between the head walk and a tail. The
// LIMIT is in the SQL — never scan past the window and clip in Go.
func (s *Store) readPage(ctx context.Context, req ReadReq, gen int64) (ReadResult, error) {
	n := s.clampLimit(req.Limit)
	var sb strings.Builder
	sb.WriteString(`SELECT seq, at, type, data, object_key, expires_at FROM notebook_entries
		WHERE notebook_id = ? AND (expires_at IS NULL OR expires_at > ?)`)
	args := []any{req.Ref.ID(), FormatAt(s.now())}
	if req.After.Seq > 0 {
		sb.WriteString(" AND seq > ?")
		args = append(args, req.After.Seq)
	}
	if req.UpTo > 0 {
		sb.WriteString(" AND seq <= ?")
		args = append(args, req.UpTo)
	}
	if !req.Since.IsZero() {
		sb.WriteString(" AND at >= ?")
		args = append(args, FormatAt(req.Since))
	}
	if !req.Until.IsZero() {
		sb.WriteString(" AND at < ?")
		args = append(args, FormatAt(req.Until))
	}
	if req.Type != "" {
		sb.WriteString(" AND type = ?")
		args = append(args, req.Type)
	}
	tail := req.Tail > 0
	lim := n + 1 // forward: one probe row past the window says "more"
	if tail {
		// The newest N of the selection. An explicit limit caps N; with
		// none, only the node ceiling does — tail=200 means 200.
		ceiling := s.limits.MaxReadLimit
		if req.Limit > 0 {
			ceiling = n
		}
		lim = min(req.Tail, ceiling)
		sb.WriteString(" ORDER BY seq DESC LIMIT ?")
	} else {
		sb.WriteString(" ORDER BY seq ASC LIMIT ?")
	}
	args = append(args, lim)

	rows, err := s.db.QueryContext(ctx, s.rb(sb.String()), args...)
	if err != nil {
		return ReadResult{}, &StoreError{Op: "read", Err: err}
	}
	defer rows.Close()
	entries := make([]Entry, 0, min(lim, 256))
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return ReadResult{}, &StoreError{Op: "read", Err: err}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return ReadResult{}, &StoreError{Op: "read", Err: err}
	}

	var res ReadResult
	if tail {
		for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
			entries[i], entries[j] = entries[j], entries[i]
		}
		res.Truncated = len(entries) == lim && req.Tail > lim
	} else if len(entries) > n {
		entries = entries[:n]
		res.Next = Cursor{Generation: gen, Seq: entries[n-1].Seq}
		res.Truncated = true
	}
	res.Entries = entries
	res.Count = len(entries)
	if len(entries) > 0 {
		res.Cursor = Cursor{Generation: gen, Seq: entries[len(entries)-1].Seq}
	}
	return res, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanEntry(r rowScanner) (Entry, error) {
	var seq int64
	var at, typ, data, key string
	var exp sql.NullString
	if err := r.Scan(&seq, &at, &typ, &data, &key, &exp); err != nil {
		return Entry{}, err
	}
	e := Entry{Seq: seq, Type: typ, Data: json.RawMessage(data), ObjectKey: key}
	e.At, _ = ParseAt(at)
	if exp.Valid {
		e.ExpiresAt, _ = ParseAt(exp.String)
	}
	return e, nil
}

// ForEach streams the selection oldest→newest without materializing more
// than one page: it snapshots the head's HighSeq as the ceiling (so a hot
// notebook cannot make an export run forever), then walks pages of
// MaxReadLimit. fn's error aborts the walk and is returned as is. A tail
// request is a single page.
func (s *Store) ForEach(ctx context.Context, req ReadReq, fn func(Entry) error) error {
	if err := s.validateRead(req); err != nil {
		return err
	}
	if req.Tail > 0 {
		r, err := s.Read(ctx, req)
		if err != nil {
			return err
		}
		for _, e := range r.Entries {
			if err := fn(e); err != nil {
				return err
			}
		}
		return nil
	}
	head, ok, err := s.stat(ctx, req.Ref)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if !req.After.IsZero() && req.After.Generation != head.Generation {
		return &StaleCursorError{}
	}
	if req.UpTo == 0 || req.UpTo > head.HighSeq {
		req.UpTo = head.HighSeq
	}
	req.Limit = s.limits.MaxReadLimit
	for {
		r, err := s.readPage(ctx, req, head.Generation)
		if err != nil {
			return err
		}
		for _, e := range r.Entries {
			if err := fn(e); err != nil {
				return err
			}
		}
		if r.Next.IsZero() {
			return nil
		}
		req.After = r.Next
	}
}

// List returns one page of the (tenant, namespace) heads whose name starts
// with prefix, ascending by name. after is the last name of the previous
// page. Prefix matching is substr equality, not LIKE ('_' is legal in a
// name) and not a byte-range on the id (wrong under a non-C Postgres
// collation).
func (s *Store) List(ctx context.Context, tenant, namespace, prefix, after string, limit int) (ListResult, error) {
	if !segOK(tenant) {
		return ListResult{}, &InvalidArgError{Reason: "invalid tenant scope"}
	}
	if !segOK(namespace) {
		return ListResult{}, &InvalidArgError{Reason: "invalid namespace"}
	}
	n := s.clampLimit(limit)
	var sb strings.Builder
	sb.WriteString(`SELECT name, generation, next_seq, ttl_secs, created_at, updated_at FROM notebooks
		WHERE tenant = ? AND namespace = ?`)
	args := []any{tenant, namespace}
	if prefix != "" {
		sb.WriteString(" AND substr(name, 1, length(?)) = ?")
		args = append(args, prefix, prefix)
	}
	if after != "" {
		sb.WriteString(" AND name > ?")
		args = append(args, after)
	}
	sb.WriteString(" ORDER BY name LIMIT ?")
	args = append(args, n+1)

	rows, err := s.db.QueryContext(ctx, s.rb(sb.String()), args...)
	if err != nil {
		return ListResult{}, &StoreError{Op: "list", Err: err}
	}
	defer rows.Close()
	heads := make([]Head, 0, min(n+1, 256))
	for rows.Next() {
		var h Head
		var nextSeq int64
		var ttl sql.NullInt64
		var created, updated string
		if err := rows.Scan(&h.Name, &h.Generation, &nextSeq, &ttl, &created, &updated); err != nil {
			return ListResult{}, &StoreError{Op: "list", Err: err}
		}
		h.HighSeq = nextSeq - 1
		if ttl.Valid && ttl.Int64 > 0 {
			h.TTL = time.Duration(ttl.Int64) * time.Second
		}
		h.CreatedAt, _ = ParseAt(created)
		h.UpdatedAt, _ = ParseAt(updated)
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, &StoreError{Op: "list", Err: err}
	}
	var res ListResult
	if len(heads) > n {
		heads = heads[:n]
		res.Next = heads[n-1].Name
	}
	res.Notebooks = heads
	res.Count = len(heads)
	return res, nil
}

// SetTTL sets the head's default retention for new entries (0 = unbounded),
// creating the head if needed. Existing entries keep their expiry.
func (s *Store) SetTTL(ctx context.Context, ref Ref, ttl time.Duration) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	ttl = s.clampTTL(ttl)
	gen, err := newGeneration()
	if err != nil {
		return &StoreError{Op: "set ttl", Err: err}
	}
	now := FormatAt(s.now())
	var secs any
	if ttl > 0 {
		secs = int64((ttl + time.Second - 1) / time.Second)
	}
	if _, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO notebooks (notebook_id, tenant, namespace, name, generation, next_seq, ttl_secs, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT (notebook_id) DO UPDATE SET ttl_secs = excluded.ttl_secs, updated_at = excluded.updated_at`),
		ref.ID(), ref.Tenant, ref.Namespace, ref.Name, gen, secs, now, now); err != nil {
		return &StoreError{Op: "set ttl", Err: err}
	}
	return nil
}

// Delete removes the notebook: its entries, then its head, in one
// transaction under the head lock, so a concurrent Append re-evaluates to
// the auto-create path with no orphan rows to collide with — and the
// recreated head carries a new generation, which is what invalidates every
// cursor issued before the delete. Returns false when there was nothing.
func (s *Store) Delete(ctx context.Context, tenant, namespace, name string) (bool, error) {
	ref := Ref{Tenant: tenant, Namespace: namespace, Name: name}
	if err := ref.Validate(); err != nil {
		return false, err
	}
	id := ref.ID()
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return false, &StoreError{Op: "delete", Err: err}
	}
	defer tx.Rollback() //nolint:errcheck
	var gen int64
	err = tx.QueryRowContext(ctx, s.rb(`SELECT generation FROM notebooks WHERE notebook_id = ?`+s.dialect.LockClause()), id).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, &StoreError{Op: "delete", Err: err}
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM notebook_entries WHERE notebook_id = ?`), id); err != nil {
		return false, &StoreError{Op: "delete", Err: err}
	}
	if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM notebooks WHERE notebook_id = ?`), id); err != nil {
		return false, &StoreError{Op: "delete", Err: err}
	}
	if err := tx.Commit(); err != nil {
		return false, &StoreError{Op: "delete", Err: err}
	}
	return true, nil
}

// Sweep deletes up to batch expired entries (expires_at <= now, the
// complement of the read predicate) and reports how many. It never touches
// the head row, so next_seq is never renumbered and an all-expired
// notebook keeps its head. Callers loop while the count equals batch.
func (s *Store) Sweep(ctx context.Context, now time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}
	res, err := s.db.ExecContext(ctx, s.rb(`
		DELETE FROM notebook_entries WHERE (notebook_id, seq) IN (
			SELECT notebook_id, seq FROM notebook_entries
			 WHERE expires_at IS NOT NULL AND expires_at <= ? LIMIT ?)`),
		FormatAt(now), batch)
	if err != nil {
		return 0, &StoreError{Op: "sweep", Err: err}
	}
	n, _ := res.RowsAffected()
	return n, nil
}
