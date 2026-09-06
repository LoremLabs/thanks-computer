// Package contacts is the durable contacts store behind the chassis
// `contacts` personality (CardDAV) and the txco://contacts/* ops: accounts,
// per-account address books, and per-book vCard objects. The object bytes
// live here, not in the blob CAS — the protocol semantics are mutable (PUT
// replaces, DELETE deletes) and the store must support real deletion.
//
// Storage is dialect-aware (registry.Dialect, the seam auth, scheduled, imap
// and calendar use); the bundled backend is a SQLite file of its own. It is
// deliberately NOT a set of runtime tables: the dbcache watcher reloads the
// whole runtime mirror on any runtime-DB write.
//
// Identity rules: an object is addressed by its resource name (the URL
// segment) and carries a UID unique within its address book. A stack put
// addresses BY UID and keeps whatever resource name the object already has,
// so a client-created card and its later re-materializations are one
// object. Unlike the calendar store, the stored bytes are the client's own
// (CRLF-normalized) — vCard has no sound canonical encoder in the library
// stack, and a contacts app expects to read back exactly what it wrote
// (photos, X- properties, groups). The ETag is their sha256; a put whose
// content differs only in REV or PRODID is a no-op, so a re-materialization
// never churns etags. Deletes leave a tombstone (deleted_at) with a bumped
// modseq so a sync-collection REPORT can be added without a schema change;
// a later put of the same resource name resurrects the row.
package contacts

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Account statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

var (
	// ErrUsernameTaken is returned by UpsertAccount when the username already
	// belongs to another tenant (usernames are globally unique: they are the
	// login identity).
	ErrUsernameTaken = errors.New("contacts: username belongs to another tenant")
	// ErrPrecondition is a failed If-Match / If-None-Match on a put.
	ErrPrecondition = errors.New("contacts: precondition failed")
	// ErrUIDConflict is a put whose UID is already held by another live
	// object in the same address book.
	ErrUIDConflict = errors.New("contacts: uid belongs to another object")
	// ErrNotFound is a missing address book or object.
	ErrNotFound = errors.New("contacts: not found")
)

var (
	addressbookNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)
	objectNameRE      = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,255}$`)
)

// ValidAddressbookName reports whether name is an address book path
// segment: a URL segment of up to 128 chars (clients mint UUIDs; ops use
// short lowercase names).
func ValidAddressbookName(name string) bool {
	return addressbookNameRE.MatchString(name) && name != "." && name != ".."
}

// ValidObjectName reports whether name is an object resource name (a URL
// segment; the conventional `.vcf` suffix is not required — clients choose).
func ValidObjectName(name string) bool {
	return objectNameRE.MatchString(name) && name != "." && name != ".."
}

// Account is a contacts_accounts row.
type Account struct {
	Tenant    string
	Username  string
	PwHash    string
	Status    string
	Policy    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Addressbook is a live contacts_addressbooks row.
type Addressbook struct {
	ID          string
	Tenant      string
	Username    string
	Name        string
	DisplayName string
	Description string
	SortOrder   int
	Policy      json.RawMessage
	SyncToken   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Object is a contacts_objects row.
type Object struct {
	AddressbookID string
	Name          string
	UID           string
	ETag          string
	VCard         []byte
	Size          int64
	Version       string   // "3.0" | "4.0"
	FN            string   // the formatted name
	Kind          string   // individual | group | org | location
	Addresses     []string // EMAIL values, lowercased, deduped, in order
	ModSeq        int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Deleted       bool
}

// PutOpts steers PutObject.
type PutOpts struct {
	// ByUID addresses the object by obj.UID: an existing live object with
	// that UID is updated in place under its own resource name; obj.Name is
	// used only when creating. This is the stack's mode.
	ByUID bool
	// IfMatch / IfNoneMatch are the client's conditional headers, unquoted
	// ("*" for IfNoneMatch means create-only).
	IfMatch     string
	IfNoneMatch string
}

// PutResult reports what PutObject did.
type PutResult struct {
	Name    string
	UID     string
	ETag    string
	Created bool
	Noop    bool
	ModSeq  int64
}

// ListOpts narrows ListObjects.
type ListOpts struct {
	SinceModSeq    int64
	IncludeDeleted bool
	Names          []string
}

// BatchResult reports what Batch did.
type BatchResult struct {
	Created int
	Updated int
	Noop    int
	Deleted int
	Missing int // deletes whose uid had no live object
	Puts    []PutResult
}

// BatchError names the entry that failed a Batch (nothing was written).
type BatchError struct {
	Index int
	Op    string // "put" | "delete"
	Err   error
}

func (e *BatchError) Error() string { return fmt.Sprintf("contacts: %s[%d]: %v", e.Op, e.Index, e.Err) }
func (e *BatchError) Unwrap() error { return e.Err }

// Store is the façade over the three tables. It carries the dialect (for
// `?`→`$n` rebinding) and a clock seam for tests.
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

// EnsureSchema creates the tables + indexes if absent. Portable DDL: TEXT
// ids (hxid), TEXT RFC3339 timestamps, JSON as TEXT, native partial
// indexes, BIGINT counters — one DDL serves SQLite and Postgres.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contacts_accounts (
			tenant     TEXT NOT NULL,
			username   TEXT NOT NULL,
			pw_hash    TEXT NOT NULL,
			status     TEXT NOT NULL DEFAULT 'active',
			policy     TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (tenant, username),
			UNIQUE (username)
		)`,
		`CREATE TABLE IF NOT EXISTS contacts_addressbooks (
			id           TEXT PRIMARY KEY,
			tenant       TEXT NOT NULL,
			username     TEXT NOT NULL,
			name         TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			description  TEXT NOT NULL DEFAULT '',
			sort_order   INTEGER NOT NULL DEFAULT 0,
			policy       TEXT NOT NULL DEFAULT '{}',
			sync_token   BIGINT NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL,
			updated_at   TEXT NOT NULL,
			deleted_at   TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS contacts_addressbooks_name_idx
			ON contacts_addressbooks (tenant, username, name) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS contacts_objects (
			addressbook_id TEXT NOT NULL,
			name           TEXT NOT NULL,
			uid            TEXT NOT NULL,
			etag           TEXT NOT NULL,
			vcard          TEXT NOT NULL,
			size           BIGINT NOT NULL,
			version        TEXT NOT NULL DEFAULT '3.0',
			fn             TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL DEFAULT 'individual',
			addresses      TEXT NOT NULL DEFAULT '[]',
			modseq         BIGINT NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL,
			updated_at     TEXT NOT NULL,
			deleted_at     TEXT,
			PRIMARY KEY (addressbook_id, name)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS contacts_objects_uid_idx
			ON contacts_objects (addressbook_id, uid) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS contacts_objects_modseq_idx
			ON contacts_objects (addressbook_id, modseq)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("contacts: ensure schema: %w", err)
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

// ---- accounts ------------------------------------------------------------

// NormalizeUsername lowercases and trims a login / op username.
func NormalizeUsername(u string) string {
	return strings.ToLower(strings.TrimSpace(u))
}

// UpsertAccount creates the account or updates it. An empty pwHash / status
// / policy leaves the stored value unchanged on update; pwHash is required
// on create. created reports whether the row was new.
func (s *Store) UpsertAccount(ctx context.Context, tenant, username, pwHash, status string, policy json.RawMessage) (created bool, err error) {
	username = NormalizeUsername(username)
	if tenant == "" || username == "" {
		return false, errors.New("contacts: empty tenant or username")
	}
	if status != "" && status != StatusActive && status != StatusDisabled {
		return false, fmt.Errorf("contacts: status must be %s or %s", StatusActive, StatusDisabled)
	}
	if len(policy) > 0 && !json.Valid(policy) {
		return false, errors.New("contacts: policy is not valid JSON")
	}
	now := fmtTime(s.now())

	var owner string
	err = s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM contacts_accounts WHERE username = ?`), username).Scan(&owner)
	exists := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if pwHash == "" {
			return false, errors.New("contacts: a new account needs a password")
		}
		if status == "" {
			status = StatusActive
		}
		if len(policy) == 0 {
			policy = json.RawMessage(`{}`)
		}
		_, ierr := s.db.ExecContext(ctx, s.rb(`
			INSERT INTO contacts_accounts (tenant, username, pw_hash, status, policy, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			tenant, username, pwHash, status, string(policy), now, now)
		switch {
		case ierr == nil:
			return true, nil
		case s.dialect.IsUniqueViolationGeneric(ierr):
			// A concurrent creator won. Re-read: ours ⇒ update; theirs ⇒ taken.
			if rerr := s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM contacts_accounts WHERE username = ?`), username).Scan(&owner); rerr != nil {
				return false, fmt.Errorf("contacts: insert account: %w", ierr)
			}
			if owner != tenant {
				return false, ErrUsernameTaken
			}
			exists = true
		default:
			return false, fmt.Errorf("contacts: insert account: %w", ierr)
		}
	case err != nil:
		return false, fmt.Errorf("contacts: lookup account: %w", err)
	}
	if exists {
		if owner != tenant {
			return false, ErrUsernameTaken
		}
		sets := []string{"updated_at = ?"}
		args := []any{now}
		if pwHash != "" {
			sets = append(sets, "pw_hash = ?")
			args = append(args, pwHash)
		}
		if status != "" {
			sets = append(sets, "status = ?")
			args = append(args, status)
		}
		if len(policy) > 0 {
			sets = append(sets, "policy = ?")
			args = append(args, string(policy))
		}
		args = append(args, tenant, username)
		if _, err := s.db.ExecContext(ctx, s.rb(
			`UPDATE contacts_accounts SET `+strings.Join(sets, ", ")+` WHERE tenant = ? AND username = ?`), args...); err != nil {
			return false, fmt.Errorf("contacts: update account: %w", err)
		}
	}
	return false, nil
}

// GetAccount looks an account up by username (the login identity).
func (s *Store) GetAccount(ctx context.Context, username string) (Account, bool, error) {
	username = NormalizeUsername(username)
	var a Account
	var policy, created, updated string
	err := s.db.QueryRowContext(ctx, s.rb(`
		SELECT tenant, username, pw_hash, status, policy, created_at, updated_at
		  FROM contacts_accounts WHERE username = ?`), username).
		Scan(&a.Tenant, &a.Username, &a.PwHash, &a.Status, &policy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("contacts: get account: %w", err)
	}
	a.Policy = json.RawMessage(policy)
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, true, nil
}

// ---- address books -------------------------------------------------------

const addressbookCols = `id, tenant, username, name, display_name, description, sort_order,
	policy, sync_token, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanAddressbook(r rowScanner) (Addressbook, error) {
	var a Addressbook
	var policy, created, updated string
	if err := r.Scan(&a.ID, &a.Tenant, &a.Username, &a.Name, &a.DisplayName, &a.Description, &a.SortOrder,
		&policy, &a.SyncToken, &created, &updated); err != nil {
		return Addressbook{}, err
	}
	a.Policy = json.RawMessage(policy)
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, nil
}

// EnsureAddressbook returns the live address book (tenant, username, name),
// creating it when absent. On an existing book every non-empty display
// field of ab (DisplayName, Description, Policy) and a non-zero SortOrder
// are applied; empty ones are left unchanged. A soft-deleted book of the
// same name is resurrected (its objects stay tombstoned). created reports a
// fresh row.
func (s *Store) EnsureAddressbook(ctx context.Context, ab Addressbook) (Addressbook, bool, error) {
	ab.Username = NormalizeUsername(ab.Username)
	ab.Name = strings.TrimSpace(ab.Name)
	if ab.Tenant == "" || ab.Username == "" {
		return Addressbook{}, false, errors.New("contacts: empty tenant or username")
	}
	if !ValidAddressbookName(ab.Name) {
		return Addressbook{}, false, fmt.Errorf("contacts: name %q must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)", ab.Name)
	}
	if len(ab.Policy) > 0 && !json.Valid(ab.Policy) {
		return Addressbook{}, false, errors.New("contacts: policy is not valid JSON")
	}
	now := fmtTime(s.now())

	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Addressbook{}, false, fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Live row, or the most recent tombstone of that name.
	var id string
	var deleted sql.NullString
	err = tx.QueryRowContext(ctx, s.rb(`
		SELECT id, deleted_at FROM contacts_addressbooks
		 WHERE tenant = ? AND username = ? AND name = ?
		 ORDER BY (deleted_at IS NULL) DESC, updated_at DESC LIMIT 1`+lock(s)),
		ab.Tenant, ab.Username, ab.Name).Scan(&id, &deleted)
	created := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = "ab_" + hxid.NewTimeSort().String()
		policy := string(ab.Policy)
		if policy == "" {
			policy = "{}"
		}
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO contacts_addressbooks (`+addressbookCols+`, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, NULL)`),
			id, ab.Tenant, ab.Username, ab.Name, ab.DisplayName, ab.Description, ab.SortOrder,
			policy, now, now); err != nil {
			return Addressbook{}, false, fmt.Errorf("contacts: insert addressbook: %w", err)
		}
		created = true
	case err != nil:
		return Addressbook{}, false, fmt.Errorf("contacts: lookup addressbook: %w", err)
	default:
		sets := []string{"updated_at = ?"}
		args := []any{now}
		if deleted.Valid {
			sets = append(sets, "deleted_at = NULL")
			created = true
		}
		if ab.DisplayName != "" {
			sets = append(sets, "display_name = ?")
			args = append(args, ab.DisplayName)
		}
		if ab.Description != "" {
			sets = append(sets, "description = ?")
			args = append(args, ab.Description)
		}
		if ab.SortOrder != 0 {
			sets = append(sets, "sort_order = ?")
			args = append(args, ab.SortOrder)
		}
		if len(ab.Policy) > 0 {
			sets = append(sets, "policy = ?")
			args = append(args, string(ab.Policy))
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx, s.rb(
			`UPDATE contacts_addressbooks SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...); err != nil {
			return Addressbook{}, false, fmt.Errorf("contacts: update addressbook: %w", err)
		}
	}
	out, err := scanAddressbook(tx.QueryRowContext(ctx, s.rb(`SELECT `+addressbookCols+` FROM contacts_addressbooks WHERE id = ?`), id))
	if err != nil {
		return Addressbook{}, false, fmt.Errorf("contacts: reread addressbook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Addressbook{}, false, fmt.Errorf("contacts: commit: %w", err)
	}
	return out, created, nil
}

// SetAddressbookProps applies a client PROPPATCH: only the fields whose
// pointer is non-nil change (an empty string clears).
func (s *Store) SetAddressbookProps(ctx context.Context, id string, displayName, description *string, sortOrder *int) error {
	sets := []string{"updated_at = ?"}
	args := []any{fmtTime(s.now())}
	if displayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *displayName)
	}
	if description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *description)
	}
	if sortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *sortOrder)
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, s.rb(`UPDATE contacts_addressbooks SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`), args...)
	if err != nil {
		return fmt.Errorf("contacts: set props: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAddressbook returns the live address book (tenant, username, name).
func (s *Store) GetAddressbook(ctx context.Context, tenant, username, name string) (Addressbook, bool, error) {
	a, err := scanAddressbook(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+addressbookCols+` FROM contacts_addressbooks
		 WHERE tenant = ? AND username = ? AND name = ? AND deleted_at IS NULL`),
		tenant, NormalizeUsername(username), name))
	if errors.Is(err, sql.ErrNoRows) {
		return Addressbook{}, false, nil
	}
	if err != nil {
		return Addressbook{}, false, fmt.Errorf("contacts: get addressbook: %w", err)
	}
	return a, true, nil
}

// GetAddressbookByID returns a live address book by id.
func (s *Store) GetAddressbookByID(ctx context.Context, id string) (Addressbook, bool, error) {
	a, err := scanAddressbook(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+addressbookCols+` FROM contacts_addressbooks WHERE id = ? AND deleted_at IS NULL`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Addressbook{}, false, nil
	}
	if err != nil {
		return Addressbook{}, false, fmt.Errorf("contacts: get addressbook: %w", err)
	}
	return a, true, nil
}

// ListAddressbooks returns the account's live address books, by sort order
// then name.
func (s *Store) ListAddressbooks(ctx context.Context, tenant, username string) ([]Addressbook, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT `+addressbookCols+` FROM contacts_addressbooks
		 WHERE tenant = ? AND username = ? AND deleted_at IS NULL
		 ORDER BY sort_order, name`), tenant, NormalizeUsername(username))
	if err != nil {
		return nil, fmt.Errorf("contacts: list addressbooks: %w", err)
	}
	defer rows.Close()
	var out []Addressbook
	for rows.Next() {
		a, err := scanAddressbook(rows)
		if err != nil {
			return nil, fmt.Errorf("contacts: scan addressbook: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RemoveAddressbook soft-deletes an address book and tombstones its live
// objects.
func (s *Store) RemoveAddressbook(ctx context.Context, id string) (bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var token int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT sync_token FROM contacts_addressbooks WHERE id = ? AND deleted_at IS NULL`+lock(s)), id).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("contacts: lock addressbook: %w", err)
	}
	token++
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE contacts_objects SET deleted_at = ?, modseq = ?, updated_at = ? WHERE addressbook_id = ? AND deleted_at IS NULL`),
		now, token, now, id); err != nil {
		return false, fmt.Errorf("contacts: tombstone objects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE contacts_addressbooks SET deleted_at = ?, sync_token = ?, updated_at = ? WHERE id = ?`),
		now, token, now, id); err != nil {
		return false, fmt.Errorf("contacts: delete addressbook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("contacts: commit: %w", err)
	}
	return true, nil
}

// ---- objects -------------------------------------------------------------

const objectCols = `addressbook_id, name, uid, etag, vcard, size, version, fn, kind, addresses,
	modseq, created_at, updated_at, deleted_at`

func scanObject(r rowScanner) (Object, error) {
	var o Object
	var vcard, addresses, created, updated string
	var deleted sql.NullString
	if err := r.Scan(&o.AddressbookID, &o.Name, &o.UID, &o.ETag, &vcard, &o.Size, &o.Version, &o.FN, &o.Kind, &addresses,
		&o.ModSeq, &created, &updated, &deleted); err != nil {
		return Object{}, err
	}
	o.VCard = []byte(vcard)
	o.Addresses = []string{}
	_ = json.Unmarshal([]byte(addresses), &o.Addresses)
	if o.Addresses == nil {
		o.Addresses = []string{}
	}
	o.CreatedAt = parseTime(created)
	o.UpdatedAt = parseTime(updated)
	o.Deleted = deleted.Valid
	return o, nil
}

// ETagOf is the etag of stored bytes: bare sha256 hex.
func ETagOf(vcard []byte) string {
	sum := sha256.Sum256(vcard)
	return hex.EncodeToString(sum[:])
}

// SameContent reports whether two vCards differ only in their REV / PRODID
// lines — the re-materialization no-op rule. Folded continuation lines are
// joined before the comparison.
func SameContent(a, b []byte) bool {
	return stripVolatile(a) == stripVolatile(b)
}

func stripVolatile(b []byte) string {
	var out []string
	for _, line := range unfold(b) {
		name := line
		if i := strings.IndexAny(name, ";:"); i >= 0 {
			name = name[:i]
		}
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		switch strings.ToUpper(name) {
		case "REV", "PRODID":
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\r\n")
}

func addressesJSON(a []string) string {
	if a == nil {
		a = []string{}
	}
	b, err := json.Marshal(a)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// PutObject stores obj (its VCard is the bytes to keep; Name, UID, Version,
// FN, Kind, Addresses are the facts the caller parsed from them). The
// address book's sync_token advances with the object's modseq in one
// transaction. See the package comment for the identity, no-op and
// tombstone rules.
func (s *Store) PutObject(ctx context.Context, abID string, obj Object, opts PutOpts) (PutResult, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return PutResult{}, fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	token, err := s.lockBook(ctx, tx, abID)
	if err != nil {
		return PutResult{}, err
	}
	before := token
	res, err := s.putTx(ctx, tx, abID, &token, now, obj, opts)
	if err != nil {
		return PutResult{}, err
	}
	if token != before {
		if err := s.advance(ctx, tx, abID, token, now); err != nil {
			return PutResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return PutResult{}, fmt.Errorf("contacts: commit: %w", err)
	}
	return res, nil
}

// Batch applies puts (each addressed by UID, the stack's mode) and deletes
// (by UID) in ONE transaction with one sync_token advance: the bounded
// write a stack without loops needs to reconcile a list. An error on any
// entry rolls the whole batch back and names the entry (*BatchError).
// Unknown uids in deletes are counted as Missing, not errors.
func (s *Store) Batch(ctx context.Context, abID string, puts []Object, deletes []string) (BatchResult, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return BatchResult{}, fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	token, err := s.lockBook(ctx, tx, abID)
	if err != nil {
		return BatchResult{}, err
	}
	before := token
	var out BatchResult
	for i, o := range puts {
		r, err := s.putTx(ctx, tx, abID, &token, now, o, PutOpts{ByUID: true})
		if err != nil {
			return BatchResult{}, &BatchError{Index: i, Op: "put", Err: err}
		}
		switch {
		case r.Noop:
			out.Noop++
		case r.Created:
			out.Created++
		default:
			out.Updated++
		}
		out.Puts = append(out.Puts, r)
	}
	for i, uid := range deletes {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			return BatchResult{}, &BatchError{Index: i, Op: "delete", Err: errors.New("empty uid")}
		}
		_, found, err := s.deleteTx(ctx, tx, abID, &token, now, "uid", uid)
		if err != nil {
			return BatchResult{}, &BatchError{Index: i, Op: "delete", Err: err}
		}
		if found {
			out.Deleted++
		} else {
			out.Missing++
		}
	}
	if token != before {
		if err := s.advance(ctx, tx, abID, token, now); err != nil {
			return BatchResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return BatchResult{}, fmt.Errorf("contacts: commit: %w", err)
	}
	return out, nil
}

type txq interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// lockBook reads (and on Postgres row-locks) the live book's sync_token.
func (s *Store) lockBook(ctx context.Context, tx txq, abID string) (int64, error) {
	var token int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT sync_token FROM contacts_addressbooks WHERE id = ? AND deleted_at IS NULL`+lock(s)), abID).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("contacts: lock addressbook: %w", err)
	}
	return token, nil
}

func (s *Store) advance(ctx context.Context, tx txq, abID string, token int64, now string) error {
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE contacts_addressbooks SET sync_token = ?, updated_at = ? WHERE id = ?`), token, now, abID); err != nil {
		return fmt.Errorf("contacts: advance sync token: %w", err)
	}
	return nil
}

// putTx is PutObject inside a write transaction whose book is locked; it
// bumps *token once when it writes.
func (s *Store) putTx(ctx context.Context, tx txq, abID string, token *int64, now string, obj Object, opts PutOpts) (PutResult, error) {
	obj.Name = strings.TrimSpace(obj.Name)
	obj.UID = strings.TrimSpace(obj.UID)
	if abID == "" || obj.UID == "" || len(obj.VCard) == 0 {
		return PutResult{}, errors.New("contacts: put needs an address book, a uid and bytes")
	}
	if obj.Name != "" && !ValidObjectName(obj.Name) {
		return PutResult{}, fmt.Errorf("contacts: resource name %q is not a URL segment", obj.Name)
	}
	if obj.Version == "" {
		obj.Version = "3.0"
	}
	if obj.Kind == "" {
		obj.Kind = "individual"
	}

	// The live object with this UID, if any — the identity a stack put
	// addresses, and the conflict a client put must not create.
	byUID, haveUID, err := s.getObjectTx(ctx, tx, abID, "uid", obj.UID, false)
	if err != nil {
		return PutResult{}, err
	}
	var target Object
	var haveTarget bool
	switch {
	case opts.ByUID && haveUID:
		target, haveTarget = byUID, true
	case opts.ByUID:
		if obj.Name == "" {
			return PutResult{}, errors.New("contacts: creating by uid needs a resource name")
		}
		fallthrough
	default:
		if obj.Name == "" {
			return PutResult{}, errors.New("contacts: put needs a resource name")
		}
		// Row under this name: live or tombstoned.
		target, haveTarget, err = s.getObjectTx(ctx, tx, abID, "name", obj.Name, true)
		if err != nil {
			return PutResult{}, err
		}
		if haveUID && (!haveTarget || target.Deleted || byUID.Name != target.Name) {
			return PutResult{}, ErrUIDConflict
		}
	}
	live := haveTarget && !target.Deleted

	// Client preconditions (unquoted etags; "*" = must not exist).
	if opts.IfNoneMatch == "*" && live {
		return PutResult{}, ErrPrecondition
	}
	if opts.IfNoneMatch != "" && opts.IfNoneMatch != "*" && live && target.ETag == opts.IfNoneMatch {
		return PutResult{}, ErrPrecondition
	}
	if opts.IfMatch != "" && (!live || (opts.IfMatch != "*" && target.ETag != opts.IfMatch)) {
		return PutResult{}, ErrPrecondition
	}

	if live && SameContent(target.VCard, obj.VCard) {
		return PutResult{Name: target.Name, UID: target.UID, ETag: target.ETag, Noop: true, ModSeq: target.ModSeq}, nil
	}

	*token++
	etag := ETagOf(obj.VCard)
	name := obj.Name
	if haveTarget {
		name = target.Name
		if _, err := tx.ExecContext(ctx, s.rb(`
			UPDATE contacts_objects SET uid = ?, etag = ?, vcard = ?, size = ?, version = ?, fn = ?, kind = ?, addresses = ?,
			       modseq = ?, updated_at = ?, deleted_at = NULL
			 WHERE addressbook_id = ? AND name = ?`),
			obj.UID, etag, string(obj.VCard), int64(len(obj.VCard)), obj.Version, obj.FN, obj.Kind, addressesJSON(obj.Addresses),
			*token, now, abID, name); err != nil {
			return PutResult{}, fmt.Errorf("contacts: update object: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO contacts_objects (`+objectCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`),
			abID, name, obj.UID, etag, string(obj.VCard), int64(len(obj.VCard)), obj.Version, obj.FN, obj.Kind, addressesJSON(obj.Addresses),
			*token, now, now); err != nil {
			if s.dialect.IsUniqueViolationGeneric(err) {
				return PutResult{}, ErrUIDConflict
			}
			return PutResult{}, fmt.Errorf("contacts: insert object: %w", err)
		}
	}
	return PutResult{Name: name, UID: obj.UID, ETag: etag, Created: !live, ModSeq: *token}, nil
}

// deleteTx tombstones the live object found by col (name | uid); it bumps
// *token when it finds one.
func (s *Store) deleteTx(ctx context.Context, tx txq, abID string, token *int64, now, col, val string) (string, bool, error) {
	o, ok, err := s.getObjectTx(ctx, tx, abID, col, val, false)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	*token++
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE contacts_objects SET deleted_at = ?, modseq = ?, updated_at = ? WHERE addressbook_id = ? AND name = ?`),
		now, *token, now, abID, o.Name); err != nil {
		return "", false, fmt.Errorf("contacts: delete object: %w", err)
	}
	return o.ETag, true, nil
}

// getObjectTx fetches one object by `name` or `uid`; withDeleted includes
// tombstones (by name only — a tombstoned uid may be reused).
func (s *Store) getObjectTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, abID, col, val string, withDeleted bool) (Object, bool, error) {
	where := `addressbook_id = ? AND ` + col + ` = ?`
	if !withDeleted {
		where += ` AND deleted_at IS NULL`
	}
	o, err := scanObject(q.QueryRowContext(ctx, s.rb(`SELECT `+objectCols+` FROM contacts_objects WHERE `+where+lock(s)), abID, val))
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, fmt.Errorf("contacts: get object: %w", err)
	}
	return o, true, nil
}

// GetObject returns the live object under a resource name.
func (s *Store) GetObject(ctx context.Context, abID, name string) (Object, bool, error) {
	return s.getObjectTx(ctx, s.db, abID, "name", name, false)
}

// GetObjectByUID returns the live object with this UID.
func (s *Store) GetObjectByUID(ctx context.Context, abID, uid string) (Object, bool, error) {
	return s.getObjectTx(ctx, s.db, abID, "uid", uid, false)
}

// ListObjects returns an address book's objects (live only unless
// IncludeDeleted), optionally after a modseq and/or restricted to names,
// by name.
func (s *Store) ListObjects(ctx context.Context, abID string, opts ListOpts) ([]Object, error) {
	where := []string{"addressbook_id = ?"}
	args := []any{abID}
	if !opts.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	if opts.SinceModSeq > 0 {
		where = append(where, "modseq > ?")
		args = append(args, opts.SinceModSeq)
	}
	if len(opts.Names) > 0 {
		ph := make([]string, len(opts.Names))
		for i, n := range opts.Names {
			ph[i] = "?"
			args = append(args, n)
		}
		where = append(where, "name IN ("+strings.Join(ph, ", ")+")")
	}
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT `+objectCols+` FROM contacts_objects WHERE `+strings.Join(where, " AND ")+` ORDER BY name`), args...)
	if err != nil {
		return nil, fmt.Errorf("contacts: list objects: %w", err)
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, fmt.Errorf("contacts: scan object: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteObject tombstones the live object under name. Returns its etag and
// whether it existed.
func (s *Store) DeleteObject(ctx context.Context, abID, name string) (string, bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return "", false, fmt.Errorf("contacts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	token, err := s.lockBook(ctx, tx, abID)
	if err != nil {
		return "", false, err
	}
	etag, found, err := s.deleteTx(ctx, tx, abID, &token, now, "name", name)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	if err := s.advance(ctx, tx, abID, token, now); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("contacts: commit: %w", err)
	}
	return etag, true, nil
}

// lock is the dialect's row-lock suffix for a SELECT inside a write tx
// ("" on SQLite, whose BEGIN IMMEDIATE already holds the write lock).
func lock(s *Store) string {
	if c := s.dialect.LockClause(); c != "" {
		return " " + c
	}
	return ""
}
