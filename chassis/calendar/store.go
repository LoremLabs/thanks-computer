// Package calendar is the durable calendar store behind the chassis
// `calendar` personality (CalDAV + ICS feeds) and the txco://calendar/* ops:
// accounts, per-account calendars, and per-calendar iCalendar objects. The
// object bytes live here, not in the blob CAS — the protocol semantics are
// mutable (PUT replaces, DELETE deletes) and the store must support real
// deletion.
//
// Storage is dialect-aware (registry.Dialect, the seam auth, scheduled and
// imap use); the bundled backend is a SQLite file of its own. It is
// deliberately NOT a set of runtime tables: the dbcache watcher reloads the
// whole runtime mirror on any runtime-DB write.
//
// Identity rules: an object is addressed by its resource name (the URL
// segment) and carries a UID unique within its calendar. A stack put
// addresses BY UID and keeps whatever resource name the object already has,
// so a client-created event and its later re-materializations are one
// object. The stored bytes are always a canonical iCalendar encoding; the
// ETag is their sha256; a put whose content differs only in DTSTAMP or
// SEQUENCE is a no-op, so an hourly re-materialization never churns etags.
// Deletes leave a tombstone (deleted_at) with a bumped modseq so a
// sync-collection REPORT can be added without a schema change; a later put
// of the same resource name resurrects the row.
package calendar

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
	ErrUsernameTaken = errors.New("calendar: username belongs to another tenant")
	// ErrPrecondition is returned by PutObject when an If-Match / If-None-Match
	// condition fails.
	ErrPrecondition = errors.New("calendar: precondition failed")
	// ErrUIDConflict is returned by PutObject when the object's UID already
	// names a different live resource in the calendar (RFC 4791 §5.3.2.1).
	ErrUIDConflict = errors.New("calendar: uid already used by another resource")
	// ErrNotFound is returned when a calendar or object does not exist.
	ErrNotFound = errors.New("calendar: not found")
)

var (
	calendarNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)
	objectNameRE   = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,255}$`)
	hexRE          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidCalendarName reports whether name is a calendar path segment: a
// URL segment of up to 128 chars (clients mint UUIDs; ops use short
// lowercase names). `feed` cannot collide — it lives at another depth.
func ValidCalendarName(name string) bool {
	return calendarNameRE.MatchString(name) && name != "." && name != ".."
}

// ValidObjectName reports whether name is an object resource name (a URL
// segment; the conventional `.ics` suffix is not required — clients choose).
func ValidObjectName(name string) bool {
	return objectNameRE.MatchString(name) && name != "." && name != ".."
}

// Account is a calendar_accounts row.
type Account struct {
	Tenant    string
	Username  string
	PwHash    string
	Status    string
	Policy    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Calendar is a live calendar_calendars row.
type Calendar struct {
	ID            string
	Tenant        string
	Username      string
	Name          string
	DisplayName   string
	Description   string
	Color         string
	SortOrder     int
	Timezone      string
	Policy        json.RawMessage
	FeedTokenHash string
	SyncToken     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Object is a calendar_objects row.
type Object struct {
	CalendarID string
	Name       string
	UID        string
	ETag       string
	Component  string
	ICal       []byte
	Size       int64
	Summary    string
	DTStartUTC string // RFC3339 UTC of the first occurrence ("" when unknown)
	DTEndUTC   string
	Recurs     bool
	Sequence   int64
	ModSeq     int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Deleted    bool
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
	Name     string
	UID      string
	ETag     string
	Created  bool
	Noop     bool
	Sequence int64
	ModSeq   int64
}

// ListOpts narrows ListObjects.
type ListOpts struct {
	SinceModSeq    int64
	IncludeDeleted bool
	Names          []string
}

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
		`CREATE TABLE IF NOT EXISTS calendar_accounts (
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
		`CREATE TABLE IF NOT EXISTS calendar_calendars (
			id              TEXT PRIMARY KEY,
			tenant          TEXT NOT NULL,
			username        TEXT NOT NULL,
			name            TEXT NOT NULL,
			display_name    TEXT NOT NULL DEFAULT '',
			description     TEXT NOT NULL DEFAULT '',
			color           TEXT NOT NULL DEFAULT '',
			sort_order      INTEGER NOT NULL DEFAULT 0,
			timezone        TEXT NOT NULL DEFAULT '',
			policy          TEXT NOT NULL DEFAULT '{}',
			feed_token_hash TEXT NOT NULL DEFAULT '',
			sync_token      BIGINT NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL,
			deleted_at      TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS calendar_calendars_name_idx
			ON calendar_calendars (tenant, username, name) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS calendar_calendars_feed_idx
			ON calendar_calendars (feed_token_hash) WHERE feed_token_hash <> ''`,
		`CREATE TABLE IF NOT EXISTS calendar_objects (
			calendar_id TEXT NOT NULL,
			name        TEXT NOT NULL,
			uid         TEXT NOT NULL,
			etag        TEXT NOT NULL,
			component   TEXT NOT NULL DEFAULT 'VEVENT',
			ical        TEXT NOT NULL,
			size        BIGINT NOT NULL,
			summary     TEXT NOT NULL DEFAULT '',
			dtstart_utc TEXT NOT NULL DEFAULT '',
			dtend_utc   TEXT NOT NULL DEFAULT '',
			recurs      INTEGER NOT NULL DEFAULT 0,
			sequence    BIGINT NOT NULL DEFAULT 0,
			modseq      BIGINT NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL,
			deleted_at  TEXT,
			PRIMARY KEY (calendar_id, name)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS calendar_objects_uid_idx
			ON calendar_objects (calendar_id, uid) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS calendar_objects_modseq_idx
			ON calendar_objects (calendar_id, modseq)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("calendar: ensure schema: %w", err)
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
		return false, errors.New("calendar: empty tenant or username")
	}
	if status != "" && status != StatusActive && status != StatusDisabled {
		return false, fmt.Errorf("calendar: status must be %s or %s", StatusActive, StatusDisabled)
	}
	if len(policy) > 0 && !json.Valid(policy) {
		return false, errors.New("calendar: policy is not valid JSON")
	}
	now := fmtTime(s.now())

	var owner string
	err = s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM calendar_accounts WHERE username = ?`), username).Scan(&owner)
	exists := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if pwHash == "" {
			return false, errors.New("calendar: a new account needs a password")
		}
		if status == "" {
			status = StatusActive
		}
		if len(policy) == 0 {
			policy = json.RawMessage(`{}`)
		}
		_, ierr := s.db.ExecContext(ctx, s.rb(`
			INSERT INTO calendar_accounts (tenant, username, pw_hash, status, policy, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			tenant, username, pwHash, status, string(policy), now, now)
		switch {
		case ierr == nil:
			return true, nil
		case s.dialect.IsUniqueViolationGeneric(ierr):
			// A concurrent creator won. Re-read: ours ⇒ update; theirs ⇒ taken.
			if rerr := s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM calendar_accounts WHERE username = ?`), username).Scan(&owner); rerr != nil {
				return false, fmt.Errorf("calendar: insert account: %w", ierr)
			}
			if owner != tenant {
				return false, ErrUsernameTaken
			}
			exists = true
		default:
			return false, fmt.Errorf("calendar: insert account: %w", ierr)
		}
	case err != nil:
		return false, fmt.Errorf("calendar: lookup account: %w", err)
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
			`UPDATE calendar_accounts SET `+strings.Join(sets, ", ")+` WHERE tenant = ? AND username = ?`), args...); err != nil {
			return false, fmt.Errorf("calendar: update account: %w", err)
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
		  FROM calendar_accounts WHERE username = ?`), username).
		Scan(&a.Tenant, &a.Username, &a.PwHash, &a.Status, &policy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("calendar: get account: %w", err)
	}
	a.Policy = json.RawMessage(policy)
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, true, nil
}

// ---- calendars -----------------------------------------------------------

const calendarCols = `id, tenant, username, name, display_name, description, color, sort_order,
	timezone, policy, feed_token_hash, sync_token, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanCalendar(r rowScanner) (Calendar, error) {
	var c Calendar
	var policy, created, updated string
	if err := r.Scan(&c.ID, &c.Tenant, &c.Username, &c.Name, &c.DisplayName, &c.Description, &c.Color, &c.SortOrder,
		&c.Timezone, &policy, &c.FeedTokenHash, &c.SyncToken, &created, &updated); err != nil {
		return Calendar{}, err
	}
	c.Policy = json.RawMessage(policy)
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}

// EnsureCalendar returns the live calendar (tenant, username, name),
// creating it when absent. On an existing calendar every non-empty display
// field of c (DisplayName, Description, Color, Timezone, Policy) and a
// non-zero SortOrder are applied; empty ones are left unchanged. A
// soft-deleted calendar of the same name is resurrected (its objects stay
// tombstoned). created reports a fresh row.
func (s *Store) EnsureCalendar(ctx context.Context, c Calendar) (Calendar, bool, error) {
	c.Username = NormalizeUsername(c.Username)
	c.Name = strings.TrimSpace(c.Name)
	if c.Tenant == "" || c.Username == "" {
		return Calendar{}, false, errors.New("calendar: empty tenant or username")
	}
	if !ValidCalendarName(c.Name) {
		return Calendar{}, false, fmt.Errorf("calendar: name %q must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)", c.Name)
	}
	if len(c.Policy) > 0 && !json.Valid(c.Policy) {
		return Calendar{}, false, errors.New("calendar: policy is not valid JSON")
	}
	now := fmtTime(s.now())

	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Live row, or the most recent tombstone of that name.
	var id string
	var deleted sql.NullString
	err = tx.QueryRowContext(ctx, s.rb(`
		SELECT id, deleted_at FROM calendar_calendars
		 WHERE tenant = ? AND username = ? AND name = ?
		 ORDER BY (deleted_at IS NULL) DESC, updated_at DESC LIMIT 1`+lock(s)),
		c.Tenant, c.Username, c.Name).Scan(&id, &deleted)
	created := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = "cal_" + hxid.NewTimeSort().String()
		policy := string(c.Policy)
		if policy == "" {
			policy = "{}"
		}
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO calendar_calendars (`+calendarCols+`, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?, NULL)`),
			id, c.Tenant, c.Username, c.Name, c.DisplayName, c.Description, c.Color, c.SortOrder,
			c.Timezone, policy, now, now); err != nil {
			return Calendar{}, false, fmt.Errorf("calendar: insert calendar: %w", err)
		}
		created = true
	case err != nil:
		return Calendar{}, false, fmt.Errorf("calendar: lookup calendar: %w", err)
	default:
		sets := []string{"updated_at = ?"}
		args := []any{now}
		if deleted.Valid {
			sets = append(sets, "deleted_at = NULL", "feed_token_hash = ''")
			created = true
		}
		if c.DisplayName != "" {
			sets = append(sets, "display_name = ?")
			args = append(args, c.DisplayName)
		}
		if c.Description != "" {
			sets = append(sets, "description = ?")
			args = append(args, c.Description)
		}
		if c.Color != "" {
			sets = append(sets, "color = ?")
			args = append(args, c.Color)
		}
		if c.SortOrder != 0 {
			sets = append(sets, "sort_order = ?")
			args = append(args, c.SortOrder)
		}
		if c.Timezone != "" {
			sets = append(sets, "timezone = ?")
			args = append(args, c.Timezone)
		}
		if len(c.Policy) > 0 {
			sets = append(sets, "policy = ?")
			args = append(args, string(c.Policy))
		}
		args = append(args, id)
		if _, err := tx.ExecContext(ctx, s.rb(
			`UPDATE calendar_calendars SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...); err != nil {
			return Calendar{}, false, fmt.Errorf("calendar: update calendar: %w", err)
		}
	}
	out, err := scanCalendar(tx.QueryRowContext(ctx, s.rb(`SELECT `+calendarCols+` FROM calendar_calendars WHERE id = ?`), id))
	if err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: reread calendar: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: commit: %w", err)
	}
	return out, created, nil
}

// SetCalendarProps applies a client PROPPATCH: only the fields whose
// pointer is non-nil change (an empty string clears).
func (s *Store) SetCalendarProps(ctx context.Context, id string, displayName, description, color *string, sortOrder *int) error {
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
	if color != nil {
		sets = append(sets, "color = ?")
		args = append(args, *color)
	}
	if sortOrder != nil {
		sets = append(sets, "sort_order = ?")
		args = append(args, *sortOrder)
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, s.rb(`UPDATE calendar_calendars SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`), args...)
	if err != nil {
		return fmt.Errorf("calendar: set props: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetCalendar returns the live calendar (tenant, username, name).
func (s *Store) GetCalendar(ctx context.Context, tenant, username, name string) (Calendar, bool, error) {
	c, err := scanCalendar(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+calendarCols+` FROM calendar_calendars
		 WHERE tenant = ? AND username = ? AND name = ? AND deleted_at IS NULL`),
		tenant, NormalizeUsername(username), name))
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, false, nil
	}
	if err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: get calendar: %w", err)
	}
	return c, true, nil
}

// GetCalendarByID returns a live calendar by id.
func (s *Store) GetCalendarByID(ctx context.Context, id string) (Calendar, bool, error) {
	c, err := scanCalendar(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+calendarCols+` FROM calendar_calendars WHERE id = ? AND deleted_at IS NULL`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, false, nil
	}
	if err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: get calendar: %w", err)
	}
	return c, true, nil
}

// CalendarByFeedHash resolves an ICS feed token (already hashed) to its
// live calendar.
func (s *Store) CalendarByFeedHash(ctx context.Context, hash string) (Calendar, bool, error) {
	if !hexRE.MatchString(hash) {
		return Calendar{}, false, nil
	}
	c, err := scanCalendar(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+calendarCols+` FROM calendar_calendars WHERE feed_token_hash = ? AND deleted_at IS NULL`), hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, false, nil
	}
	if err != nil {
		return Calendar{}, false, fmt.Errorf("calendar: get calendar by feed: %w", err)
	}
	return c, true, nil
}

// SetFeedToken stores the sha256 hex of a feed token ("" disables the feed).
func (s *Store) SetFeedToken(ctx context.Context, id, hash string) error {
	if hash != "" && !hexRE.MatchString(hash) {
		return errors.New("calendar: feed token hash must be 64 hex chars")
	}
	res, err := s.db.ExecContext(ctx, s.rb(`UPDATE calendar_calendars SET feed_token_hash = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		hash, fmtTime(s.now()), id)
	if err != nil {
		return fmt.Errorf("calendar: set feed token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCalendars returns the account's live calendars, by sort order then name.
func (s *Store) ListCalendars(ctx context.Context, tenant, username string) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT `+calendarCols+` FROM calendar_calendars
		 WHERE tenant = ? AND username = ? AND deleted_at IS NULL
		 ORDER BY sort_order, name`), tenant, NormalizeUsername(username))
	if err != nil {
		return nil, fmt.Errorf("calendar: list calendars: %w", err)
	}
	defer rows.Close()
	var out []Calendar
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, fmt.Errorf("calendar: scan calendar: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RemoveCalendar soft-deletes a calendar and tombstones its live objects.
func (s *Store) RemoveCalendar(ctx context.Context, id string) (bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("calendar: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var token int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT sync_token FROM calendar_calendars WHERE id = ? AND deleted_at IS NULL`+lock(s)), id).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("calendar: lock calendar: %w", err)
	}
	token++
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE calendar_objects SET deleted_at = ?, modseq = ?, updated_at = ? WHERE calendar_id = ? AND deleted_at IS NULL`),
		now, token, now, id); err != nil {
		return false, fmt.Errorf("calendar: tombstone objects: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE calendar_calendars SET deleted_at = ?, sync_token = ?, updated_at = ?, feed_token_hash = '' WHERE id = ?`),
		now, token, now, id); err != nil {
		return false, fmt.Errorf("calendar: delete calendar: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("calendar: commit: %w", err)
	}
	return true, nil
}

// ---- objects -------------------------------------------------------------

const objectCols = `calendar_id, name, uid, etag, component, ical, size, summary, dtstart_utc, dtend_utc,
	recurs, sequence, modseq, created_at, updated_at, deleted_at`

func scanObject(r rowScanner) (Object, error) {
	var o Object
	var ical, created, updated string
	var recurs int
	var deleted sql.NullString
	if err := r.Scan(&o.CalendarID, &o.Name, &o.UID, &o.ETag, &o.Component, &ical, &o.Size, &o.Summary, &o.DTStartUTC, &o.DTEndUTC,
		&recurs, &o.Sequence, &o.ModSeq, &created, &updated, &deleted); err != nil {
		return Object{}, err
	}
	o.ICal = []byte(ical)
	o.Recurs = recurs != 0
	o.CreatedAt = parseTime(created)
	o.UpdatedAt = parseTime(updated)
	o.Deleted = deleted.Valid
	return o, nil
}

// ETagOf is the etag of a canonical encoding: bare sha256 hex.
func ETagOf(ical []byte) string {
	sum := sha256.Sum256(ical)
	return hex.EncodeToString(sum[:])
}

// SameContent reports whether two canonical encodings differ only in their
// DTSTAMP / SEQUENCE / LAST-MODIFIED lines — the re-materialization no-op
// rule. Folded continuation lines are joined before the comparison.
func SameContent(a, b []byte) bool {
	return stripVolatile(a) == stripVolatile(b)
}

func stripVolatile(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n ", "")
	s = strings.ReplaceAll(s, "\r\n\t", "")
	var out []string
	for _, line := range strings.Split(s, "\r\n") {
		up := strings.ToUpper(line)
		if strings.HasPrefix(up, "DTSTAMP") || strings.HasPrefix(up, "SEQUENCE") || strings.HasPrefix(up, "LAST-MODIFIED") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\r\n")
}

// PutObject stores obj (its ICal is the canonical encoding; Name, UID,
// Component, Summary, DTStartUTC, DTEndUTC, Recurs, Sequence are the facts
// the caller parsed from it). The calendar's sync_token advances with the
// object's modseq in one transaction. See the package comment for the
// identity, no-op and tombstone rules.
func (s *Store) PutObject(ctx context.Context, calID string, obj Object, opts PutOpts) (PutResult, error) {
	obj.Name = strings.TrimSpace(obj.Name)
	obj.UID = strings.TrimSpace(obj.UID)
	if calID == "" || obj.UID == "" || len(obj.ICal) == 0 {
		return PutResult{}, errors.New("calendar: put needs a calendar, a uid and bytes")
	}
	if obj.Name != "" && !ValidObjectName(obj.Name) {
		return PutResult{}, fmt.Errorf("calendar: resource name %q is not a URL segment", obj.Name)
	}
	if obj.Component == "" {
		obj.Component = "VEVENT"
	}
	now := fmtTime(s.now())

	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return PutResult{}, fmt.Errorf("calendar: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var token int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT sync_token FROM calendar_calendars WHERE id = ? AND deleted_at IS NULL`+lock(s)), calID).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PutResult{}, ErrNotFound
		}
		return PutResult{}, fmt.Errorf("calendar: lock calendar: %w", err)
	}

	// The live object with this UID, if any — the identity a stack put
	// addresses, and the conflict a client put must not create.
	byUID, haveUID, err := s.getObjectTx(ctx, tx, calID, "uid", obj.UID, false)
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
			return PutResult{}, errors.New("calendar: creating by uid needs a resource name")
		}
		fallthrough
	default:
		if obj.Name == "" {
			return PutResult{}, errors.New("calendar: put needs a resource name")
		}
		// Row under this name: live or tombstoned.
		target, haveTarget, err = s.getObjectTx(ctx, tx, calID, "name", obj.Name, true)
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

	if live && SameContent(target.ICal, obj.ICal) {
		return PutResult{Name: target.Name, UID: target.UID, ETag: target.ETag, Noop: true, Sequence: target.Sequence, ModSeq: target.ModSeq}, nil
	}

	token++
	etag := ETagOf(obj.ICal)
	recurs := 0
	if obj.Recurs {
		recurs = 1
	}
	name := obj.Name
	if haveTarget {
		name = target.Name
		if _, err := tx.ExecContext(ctx, s.rb(`
			UPDATE calendar_objects SET uid = ?, etag = ?, component = ?, ical = ?, size = ?, summary = ?,
			       dtstart_utc = ?, dtend_utc = ?, recurs = ?, sequence = ?, modseq = ?, updated_at = ?, deleted_at = NULL
			 WHERE calendar_id = ? AND name = ?`),
			obj.UID, etag, obj.Component, string(obj.ICal), int64(len(obj.ICal)), obj.Summary,
			obj.DTStartUTC, obj.DTEndUTC, recurs, obj.Sequence, token, now, calID, name); err != nil {
			return PutResult{}, fmt.Errorf("calendar: update object: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, s.rb(`
			INSERT INTO calendar_objects (`+objectCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`),
			calID, name, obj.UID, etag, obj.Component, string(obj.ICal), int64(len(obj.ICal)), obj.Summary,
			obj.DTStartUTC, obj.DTEndUTC, recurs, obj.Sequence, token, now, now); err != nil {
			if s.dialect.IsUniqueViolationGeneric(err) {
				return PutResult{}, ErrUIDConflict
			}
			return PutResult{}, fmt.Errorf("calendar: insert object: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE calendar_calendars SET sync_token = ?, updated_at = ? WHERE id = ?`), token, now, calID); err != nil {
		return PutResult{}, fmt.Errorf("calendar: advance sync token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PutResult{}, fmt.Errorf("calendar: commit: %w", err)
	}
	return PutResult{Name: name, UID: obj.UID, ETag: etag, Created: !live, Sequence: obj.Sequence, ModSeq: token}, nil
}

// getObjectTx fetches one object by `name` or `uid`; withDeleted includes
// tombstones (by name only — a tombstoned uid may be reused).
func (s *Store) getObjectTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, calID, col, val string, withDeleted bool) (Object, bool, error) {
	where := `calendar_id = ? AND ` + col + ` = ?`
	if !withDeleted {
		where += ` AND deleted_at IS NULL`
	}
	o, err := scanObject(q.QueryRowContext(ctx, s.rb(`SELECT `+objectCols+` FROM calendar_objects WHERE `+where+lock(s)), calID, val))
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, false, nil
	}
	if err != nil {
		return Object{}, false, fmt.Errorf("calendar: get object: %w", err)
	}
	return o, true, nil
}

// GetObject returns the live object under a resource name.
func (s *Store) GetObject(ctx context.Context, calID, name string) (Object, bool, error) {
	return s.getObjectTx(ctx, s.db, calID, "name", name, false)
}

// GetObjectByUID returns the live object with this UID.
func (s *Store) GetObjectByUID(ctx context.Context, calID, uid string) (Object, bool, error) {
	return s.getObjectTx(ctx, s.db, calID, "uid", uid, false)
}

// ListObjects returns a calendar's objects (live only unless IncludeDeleted),
// optionally after a modseq and/or restricted to names, by name.
func (s *Store) ListObjects(ctx context.Context, calID string, opts ListOpts) ([]Object, error) {
	where := []string{"calendar_id = ?"}
	args := []any{calID}
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
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT `+objectCols+` FROM calendar_objects WHERE `+strings.Join(where, " AND ")+` ORDER BY name`), args...)
	if err != nil {
		return nil, fmt.Errorf("calendar: list objects: %w", err)
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, fmt.Errorf("calendar: scan object: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ObjectsInRange is the coarse time-range prefilter: recurring objects and
// objects with unknown bounds always pass; the rest by UTC overlap. The
// caller applies the exact match (recurrence expansion) on the result.
func (s *Store) ObjectsInRange(ctx context.Context, calID string, start, end time.Time) ([]Object, error) {
	where := []string{"calendar_id = ?", "deleted_at IS NULL"}
	args := []any{calID}
	if !start.IsZero() || !end.IsZero() {
		cond := "(recurs = 1 OR dtstart_utc = ''"
		if !end.IsZero() {
			cond += " OR dtstart_utc <= ?"
			args = append(args, fmtTime(end))
		}
		cond += ")"
		where = append(where, cond)
		if !start.IsZero() {
			where = append(where, "(recurs = 1 OR dtend_utc = '' OR dtend_utc >= ?)")
			args = append(args, fmtTime(start))
		}
	}
	rows, err := s.db.QueryContext(ctx, s.rb(`SELECT `+objectCols+` FROM calendar_objects WHERE `+strings.Join(where, " AND ")+` ORDER BY name`), args...)
	if err != nil {
		return nil, fmt.Errorf("calendar: range objects: %w", err)
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			return nil, fmt.Errorf("calendar: scan object: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteObject tombstones the live object under name. Returns its etag and
// whether it existed.
func (s *Store) DeleteObject(ctx context.Context, calID, name string) (string, bool, error) {
	now := fmtTime(s.now())
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return "", false, fmt.Errorf("calendar: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var token int64
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT sync_token FROM calendar_calendars WHERE id = ? AND deleted_at IS NULL`+lock(s)), calID).Scan(&token); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, fmt.Errorf("calendar: lock calendar: %w", err)
	}
	o, ok, err := s.getObjectTx(ctx, tx, calID, "name", name, false)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	token++
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE calendar_objects SET deleted_at = ?, modseq = ?, updated_at = ? WHERE calendar_id = ? AND name = ?`),
		now, token, now, calID, name); err != nil {
		return "", false, fmt.Errorf("calendar: delete object: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`UPDATE calendar_calendars SET sync_token = ?, updated_at = ? WHERE id = ?`), token, now, calID); err != nil {
		return "", false, fmt.Errorf("calendar: advance sync token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("calendar: commit: %w", err)
	}
	return o.ETag, true, nil
}

// lock is the dialect's row-lock suffix for a SELECT inside a write tx
// ("" on SQLite, whose BEGIN IMMEDIATE already holds the write lock).
func lock(s *Store) string {
	if c := s.dialect.LockClause(); c != "" {
		return " " + c
	}
	return ""
}
