// Package imap is the durable mailbox store behind the chassis `imap`
// personality and the txco://imap/* ops: accounts, a per-account tree of
// mailboxes, and per-mailbox UID-ordered message rows whose retained
// object (a canonical Record, or verbatim RFC 5322 bytes) lives in the
// filecas by sha256. Everything IMAP needs to serve a message without
// parsing it — ENVELOPE, BODYSTRUCTURE, size, flags — is cached on the row
// at append.
//
// Storage is dialect-aware (registry.Dialect, the same seam auth and the
// scheduled store use); the bundled backend is a SQLite file of its own.
// It is deliberately NOT a set of runtime tables: the dbcache watcher
// reloads the whole runtime mirror on any runtime-DB write, and a mailbox
// index is written on every \Seen.
//
// Immutability rules (the design doc's §25.6): bytes under a UID never
// change; same object_key + same sha is a no-op; a different sha is a NEW
// UID with the old one expunged; uidnext is stored, never derived;
// uidvalidity changes only on an explicit reset.
package imap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Kinds of retained object.
const (
	KindRecord   = "record"
	KindVerbatim = "verbatim"
)

// Account statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// ErrUsernameTaken is returned by UpsertAccount when the username already
// belongs to another tenant (usernames are globally unique: they are the
// LOGIN identity).
var ErrUsernameTaken = errors.New("imap: username belongs to another tenant")

// Account is an imap_accounts row.
type Account struct {
	Tenant    string
	Username  string
	PwHash    string
	Status    string
	Policy    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Mailbox is an imap_mailboxes row. Name is the full '/'-delimited path;
// Role is the stack's opaque stable key (survives RENAME); Attrs are
// special-use attributes.
type Mailbox struct {
	ID          string
	Tenant      string
	Username    string
	Name        string
	Role        string
	Attrs       []string
	Policy      json.RawMessage
	UIDValidity uint32
	UIDNext     uint32
	ModSeq      int64
	Subscribed  bool
	CreatedAt   time.Time
}

// Message is an imap_messages row.
type Message struct {
	MailboxID     string
	UID           uint32
	ObjectKey     string
	Kind          string
	SHA256        string
	FormatVersion int
	Size          int64
	InternalDate  time.Time
	Flags         []string
	Envelope      json.RawMessage
	BodyStructure json.RawMessage
	Subject       string
	FromAddr      string
	TextExcerpt   string
	Parts         json.RawMessage
	ModSeq        int64
	State         string
	CreatedAt     time.Time
}

// MessageHead is the per-message slice a session needs at SELECT/STATUS
// time: identity, flags, size, date — never the cached structures.
type MessageHead struct {
	UID          uint32
	Flags        []string
	Size         int64
	InternalDate time.Time
	// ModSeq is the mailbox modseq at the row's last change (append or
	// flags); the head diffs snapshots by it.
	ModSeq int64
}

// rowQuerier is what listHeads needs: *sql.DB or *sql.Tx.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// AppendResult reports what AppendMessage did.
type AppendResult struct {
	UID         uint32
	UIDValidity uint32
	// Noop: the mailbox already held this object_key with this sha.
	Noop bool
	// Replaced: the object_key existed with a different sha; ReplacedUID
	// was expunged and UID is the new row.
	Replaced    bool
	ReplacedUID uint32
}

// ChangeKind labels a post-commit change notification.
type ChangeKind string

const (
	ChangeAppend  ChangeKind = "append"
	ChangeExpunge ChangeKind = "expunge"
	ChangeFlags   ChangeKind = "flags"
)

// Change is a post-commit notification the head turns into IDLE/Poll
// updates. Seq is the message's sequence number in UID order at the moment
// of the change (for an expunge: its former position); Total is the live
// count after the change.
type Change struct {
	MailboxID string
	Kind      ChangeKind
	UID       uint32
	Seq       uint32
	Total     uint32
	Flags     []string
	// Origin is whatever WithOrigin put on the writing context (the head
	// passes its session tracker so a session is not told about its own
	// STORE twice). nil for op-side writes.
	Origin any
}

type ctxKeyOrigin struct{}

// WithOrigin tags a context with the writer's identity; the resulting
// Change carries it as Origin.
func WithOrigin(ctx context.Context, origin any) context.Context {
	return context.WithValue(ctx, ctxKeyOrigin{}, origin)
}

func originFrom(ctx context.Context) any { return ctx.Value(ctxKeyOrigin{}) }

// Store is the façade over the three tables. It carries the dialect (for
// `?`→`$n` rebinding) and a clock seam for tests, mirroring scheduled.Store.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time

	mu       sync.RWMutex
	onChange func(Change)
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

// SetOnChange installs the post-commit change listener (the head's
// notification hub). Called synchronously after each committing write, in
// order; the listener must not block.
func (s *Store) SetOnChange(fn func(Change)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *Store) emit(changes []Change) {
	s.mu.RLock()
	fn := s.onChange
	s.mu.RUnlock()
	if fn == nil {
		return
	}
	for _, c := range changes {
		fn(c)
	}
}

// EnsureSchema creates the tables + indexes if absent. Portable DDL: TEXT
// ids (hxid) instead of engine-specific autoincrement, TEXT RFC3339
// timestamps, JSON as TEXT, native partial indexes, and BIGINT for the
// uint32/int64 counters (uidvalidity is a Unix timestamp, uid copies
// uidnext, size and modseq are int64) — SQLite reads BIGINT as INTEGER
// affinity, Postgres as int8, so one DDL serves both engines.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS imap_accounts (
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
		`CREATE TABLE IF NOT EXISTS imap_mailboxes (
			id          TEXT PRIMARY KEY,
			tenant      TEXT NOT NULL,
			username    TEXT NOT NULL,
			name        TEXT NOT NULL,
			role        TEXT NOT NULL DEFAULT '',
			attrs       TEXT NOT NULL DEFAULT '[]',
			policy      TEXT NOT NULL DEFAULT '{}',
			uidvalidity BIGINT NOT NULL,
			uidnext     BIGINT NOT NULL DEFAULT 1,
			modseq      BIGINT NOT NULL DEFAULT 0,
			subscribed  INTEGER NOT NULL DEFAULT 1,
			created_at  TEXT NOT NULL,
			deleted_at  TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS imap_mailboxes_name_idx
			ON imap_mailboxes (tenant, username, name) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS imap_messages (
			mailbox_id     TEXT NOT NULL,
			uid            BIGINT NOT NULL,
			object_key     TEXT NOT NULL DEFAULT '',
			kind           TEXT NOT NULL,
			sha256         TEXT NOT NULL,
			format_version INTEGER NOT NULL DEFAULT 0,
			size           BIGINT NOT NULL,
			internaldate   TEXT NOT NULL,
			flags          TEXT NOT NULL DEFAULT '[]',
			envelope       TEXT NOT NULL DEFAULT '{}',
			bodystructure  TEXT NOT NULL DEFAULT '{}',
			subject        TEXT NOT NULL DEFAULT '',
			from_addr      TEXT NOT NULL DEFAULT '',
			text_excerpt   TEXT NOT NULL DEFAULT '',
			parts          TEXT NOT NULL DEFAULT '[]',
			modseq         BIGINT NOT NULL DEFAULT 0,
			state          TEXT NOT NULL DEFAULT 'live',
			created_at     TEXT NOT NULL,
			PRIMARY KEY (mailbox_id, uid)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS imap_messages_objkey_idx
			ON imap_messages (mailbox_id, object_key) WHERE object_key <> ''`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("imap: ensure schema: %w", err)
		}
	}
	return nil
}

// ---- accounts ------------------------------------------------------------

// NormalizeUsername lowercases and trims a LOGIN / op username. Usernames
// are e-mail addresses; the domain half is case-insensitive by RFC and the
// local part is treated so here (one account per mailbox name).
func NormalizeUsername(u string) string {
	return strings.ToLower(strings.TrimSpace(u))
}

// UpsertAccount creates the account (and its INBOX) or updates it. An empty
// pwHash / status / policy leaves the stored value unchanged on update;
// pwHash is required on create. created reports whether the row was new.
func (s *Store) UpsertAccount(ctx context.Context, tenant, username, pwHash, status string, policy json.RawMessage) (created bool, err error) {
	username = NormalizeUsername(username)
	if tenant == "" || username == "" {
		return false, errors.New("imap: empty tenant or username")
	}
	if status != "" && status != StatusActive && status != StatusDisabled {
		return false, fmt.Errorf("imap: status must be %s or %s", StatusActive, StatusDisabled)
	}
	if len(policy) > 0 && !json.Valid(policy) {
		return false, errors.New("imap: policy is not valid JSON")
	}
	now := s.now().Format(time.RFC3339)

	var owner string
	err = s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM imap_accounts WHERE username = ?`), username).Scan(&owner)
	exists := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if pwHash == "" {
			return false, errors.New("imap: a new account needs a password")
		}
		if status == "" {
			status = StatusActive
		}
		if len(policy) == 0 {
			policy = json.RawMessage(`{}`)
		}
		_, ierr := s.db.ExecContext(ctx, s.rb(`
			INSERT INTO imap_accounts (tenant, username, pw_hash, status, policy, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			tenant, username, pwHash, status, string(policy), now, now)
		switch {
		case ierr == nil:
			created = true
		case s.dialect.IsUniqueViolationGeneric(ierr):
			// A concurrent creator won (two nodes provisioning the same
			// address). Re-read: ours ⇒ treat as an update; theirs ⇒ taken.
			if rerr := s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM imap_accounts WHERE username = ?`), username).Scan(&owner); rerr != nil {
				return false, fmt.Errorf("imap: insert account: %w", ierr)
			}
			if owner != tenant {
				return false, ErrUsernameTaken
			}
			exists = true
		default:
			return false, fmt.Errorf("imap: insert account: %w", ierr)
		}
	case err != nil:
		return false, fmt.Errorf("imap: lookup account: %w", err)
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
			`UPDATE imap_accounts SET `+strings.Join(sets, ", ")+` WHERE tenant = ? AND username = ?`), args...); err != nil {
			return false, fmt.Errorf("imap: update account: %w", err)
		}
	}
	// RFC 3501 §5.1: INBOX always exists. Idempotent.
	if _, _, err := s.EnsureMailbox(ctx, tenant, username, "INBOX"); err != nil {
		return created, err
	}
	return created, nil
}

// GetAccount looks an account up by username (the LOGIN identity).
func (s *Store) GetAccount(ctx context.Context, username string) (Account, bool, error) {
	username = NormalizeUsername(username)
	var a Account
	var policy, created, updated string
	err := s.db.QueryRowContext(ctx, s.rb(`
		SELECT tenant, username, pw_hash, status, policy, created_at, updated_at
		  FROM imap_accounts WHERE username = ?`), username).
		Scan(&a.Tenant, &a.Username, &a.PwHash, &a.Status, &policy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("imap: get account: %w", err)
	}
	a.Policy = json.RawMessage(policy)
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, true, nil
}

// GetAccountByLocalPart resolves a bare local part ("paris") the way a
// mail client that drops the domain expects: the account whose username
// is <local>@<anything>, provided exactly one exists. n is the number of
// candidates, so a caller can tell "no such user" from "ambiguous" (both
// are a plain authentication failure on the wire).
func (s *Store) GetAccountByLocalPart(ctx context.Context, local string) (Account, int, error) {
	local = NormalizeUsername(local)
	if local == "" || strings.Contains(local, "@") {
		return Account{}, 0, nil
	}
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT username FROM imap_accounts
		 WHERE substr(username, 1, length(?) + 1) = ? || '@'
		 ORDER BY username LIMIT 2`), local, local)
	if err != nil {
		return Account{}, 0, fmt.Errorf("imap: lookup by local part: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return Account{}, 0, fmt.Errorf("imap: scan: %w", err)
		}
		names = append(names, u)
	}
	if err := rows.Err(); err != nil {
		return Account{}, 0, err
	}
	if len(names) != 1 {
		return Account{}, len(names), nil
	}
	a, ok, err := s.GetAccount(ctx, names[0])
	if err != nil || !ok {
		return Account{}, 0, err
	}
	return a, 1, nil
}

// ---- mailboxes -----------------------------------------------------------

// NormalizeMailboxName canonicalises a mailbox path: trims, collapses the
// delimiter, and maps any case of "inbox" to INBOX (RFC 3501 §5.1).
func NormalizeMailboxName(name string) string {
	name = strings.Trim(strings.TrimSpace(name), "/")
	for strings.Contains(name, "//") {
		name = strings.ReplaceAll(name, "//", "/")
	}
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// EnsureMailbox returns the live mailbox with this name, creating it when
// absent. created reports a fresh row. UIDVALIDITY is the creation second
// (monotonic across delete-and-recreate at one-second resolution, the
// conventional choice).
func (s *Store) EnsureMailbox(ctx context.Context, tenant, username, name string) (Mailbox, bool, error) {
	username = NormalizeUsername(username)
	name = NormalizeMailboxName(name)
	if name == "" {
		return Mailbox{}, false, errors.New("imap: empty mailbox name")
	}
	if mb, ok, err := s.GetMailbox(ctx, tenant, username, name); err != nil || ok {
		return mb, false, err
	}
	now := s.now()
	uidv := uint32(now.Unix())
	if uidv == 0 {
		uidv = 1
	}
	id := "mbox_" + hxid.NewTimeSort().String()
	if _, err := s.db.ExecContext(ctx, s.rb(`
		INSERT INTO imap_mailboxes (id, tenant, username, name, role, attrs, policy, uidvalidity, uidnext, modseq, subscribed, created_at)
		VALUES (?, ?, ?, ?, '', '[]', '{}', ?, 1, 0, 1, ?)`),
		id, tenant, username, name, uidv, now.Format(time.RFC3339)); err != nil {
		// A concurrent creator may have won the unique index; read it back.
		if s.dialect.IsUniqueViolationGeneric(err) {
			if mb, ok, gerr := s.GetMailbox(ctx, tenant, username, name); gerr == nil && ok {
				return mb, false, nil
			}
		}
		return Mailbox{}, false, fmt.Errorf("imap: insert mailbox: %w", err)
	}
	mb, _, err := s.GetMailbox(ctx, tenant, username, name)
	return mb, true, err
}

const mailboxCols = `id, tenant, username, name, role, attrs, policy, uidvalidity, uidnext, modseq, subscribed, created_at`

func scanMailbox(row interface{ Scan(...any) error }) (Mailbox, error) {
	var mb Mailbox
	var attrs, policy, created string
	var subscribed int
	if err := row.Scan(&mb.ID, &mb.Tenant, &mb.Username, &mb.Name, &mb.Role, &attrs, &policy,
		&mb.UIDValidity, &mb.UIDNext, &mb.ModSeq, &subscribed, &created); err != nil {
		return Mailbox{}, err
	}
	_ = json.Unmarshal([]byte(attrs), &mb.Attrs)
	mb.Policy = json.RawMessage(policy)
	mb.Subscribed = subscribed != 0
	mb.CreatedAt = parseTime(created)
	return mb, nil
}

// GetMailbox returns the live mailbox at name.
func (s *Store) GetMailbox(ctx context.Context, tenant, username, name string) (Mailbox, bool, error) {
	username = NormalizeUsername(username)
	name = NormalizeMailboxName(name)
	mb, err := scanMailbox(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+mailboxCols+` FROM imap_mailboxes
		 WHERE tenant = ? AND username = ? AND name = ? AND deleted_at IS NULL`), tenant, username, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, false, nil
	}
	if err != nil {
		return Mailbox{}, false, fmt.Errorf("imap: get mailbox: %w", err)
	}
	return mb, true, nil
}

// GetMailboxByRole returns the tenant/account's live mailbox carrying role
// (the stack's stable key). The first by name wins if several share it.
func (s *Store) GetMailboxByRole(ctx context.Context, tenant, username, role string) (Mailbox, bool, error) {
	username = NormalizeUsername(username)
	mb, err := scanMailbox(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+mailboxCols+` FROM imap_mailboxes
		 WHERE tenant = ? AND username = ? AND role = ? AND deleted_at IS NULL
		 ORDER BY name LIMIT 1`), tenant, username, role))
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, false, nil
	}
	if err != nil {
		return Mailbox{}, false, fmt.Errorf("imap: get mailbox by role: %w", err)
	}
	return mb, true, nil
}

// GetMailboxByID returns a live mailbox by row id.
func (s *Store) GetMailboxByID(ctx context.Context, id string) (Mailbox, bool, error) {
	mb, err := scanMailbox(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+mailboxCols+` FROM imap_mailboxes WHERE id = ? AND deleted_at IS NULL`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return Mailbox{}, false, nil
	}
	if err != nil {
		return Mailbox{}, false, fmt.Errorf("imap: get mailbox by id: %w", err)
	}
	return mb, true, nil
}

// ListMailboxes returns the account's live mailboxes sorted by name.
func (s *Store) ListMailboxes(ctx context.Context, tenant, username string) ([]Mailbox, error) {
	username = NormalizeUsername(username)
	rows, err := s.db.QueryContext(ctx, s.rb(`
		SELECT `+mailboxCols+` FROM imap_mailboxes
		 WHERE tenant = ? AND username = ? AND deleted_at IS NULL ORDER BY name`), tenant, username)
	if err != nil {
		return nil, fmt.Errorf("imap: list mailboxes: %w", err)
	}
	defer rows.Close()
	var out []Mailbox
	for rows.Next() {
		mb, err := scanMailbox(rows)
		if err != nil {
			return nil, fmt.Errorf("imap: scan mailbox: %w", err)
		}
		out = append(out, mb)
	}
	return out, rows.Err()
}

// SetSubscribed persists the SUBSCRIBE/UNSUBSCRIBE state.
func (s *Store) SetSubscribed(ctx context.Context, mailboxID string, subscribed bool) error {
	v := 0
	if subscribed {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, s.rb(`UPDATE imap_mailboxes SET subscribed = ? WHERE id = ?`), v, mailboxID)
	if err != nil {
		return fmt.Errorf("imap: set subscribed: %w", err)
	}
	return nil
}

// ---- messages ------------------------------------------------------------

// systemFlags maps the lowercase form of each RFC 3501 system flag to its
// conventional spelling — with and without the leading backslash, so a rule
// author can write `"Flagged"` in txcl (where a backslash in a string
// literal is a hazard) and store the same `\Flagged` a client sets.
var systemFlags = map[string]string{
	`\seen`: `\Seen`, `\answered`: `\Answered`, `\flagged`: `\Flagged`,
	`\deleted`: `\Deleted`, `\draft`: `\Draft`, `\recent`: `\Recent`,
	`seen`: `\Seen`, `answered`: `\Answered`, `flagged`: `\Flagged`,
	`deleted`: `\Deleted`, `draft`: `\Draft`, `recent`: `\Recent`,
}

// NormalizeFlags trims, drops empties and \Recent (server-owned, never
// stored), dedupes case-insensitively, canonicalises system-flag spelling
// and sorts — so a flags column compares byte-wise.
func NormalizeFlags(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, f := range in {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		lc := strings.ToLower(f)
		if canon, ok := systemFlags[lc]; ok {
			f = canon
			lc = strings.ToLower(canon) // "flagged" and `\flagged` are one flag
		}
		if lc == `\recent` {
			continue
		}
		if seen[lc] {
			continue
		}
		seen[lc] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// HasFlag reports whether flags contains f, case-insensitively.
func HasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}

func flagsJSON(flags []string) string {
	b, _ := json.Marshal(NormalizeFlags(flags))
	if len(b) == 0 {
		return "[]"
	}
	return string(b)
}

func rawOr(r json.RawMessage, def string) string {
	if len(r) == 0 || !json.Valid(r) {
		return def
	}
	return string(r)
}

// AppendMessage stores a message row in the mailbox and returns its UID.
// The retained object must already be in the CAS (write order is CAS → sha
// rows → index row, so a crash between leaves nothing dangling that a
// retry can't heal). Semantics by object_key (§25.6): same key + same sha
// → Noop; same key + different sha → the old row is expunged and a NEW UID
// allocated (Replaced); empty key → always a new row.
//
// Appends to one mailbox are serialized: the transaction first locks the
// mailbox row (LockClause — FOR UPDATE on Postgres; on SQLite the
// _txlock=immediate connection already holds the write lock), so the
// object_key check and the uidnext allocation can never interleave across
// nodes. A unique-index violation (a concurrent writer that won the same
// key between an out-of-tx check and this call, e.g. CopyMessage) is
// retried once.
func (s *Store) AppendMessage(ctx context.Context, mailboxID string, m Message) (AppendResult, error) {
	if mailboxID == "" {
		return AppendResult{}, errors.New("imap: empty mailbox id")
	}
	if m.SHA256 == "" || m.Kind == "" {
		return AppendResult{}, errors.New("imap: message needs kind and sha256")
	}
	if m.InternalDate.IsZero() {
		m.InternalDate = s.now()
	}
	if m.State == "" {
		m.State = "live"
	}
	var res AppendResult
	var changes []Change
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		res, changes, err = s.appendOnce(ctx, mailboxID, m)
		if err == nil || !s.dialect.IsUniqueViolationGeneric(err) {
			break
		}
	}
	if err != nil {
		return AppendResult{}, err
	}
	s.emit(changes)
	return res, nil
}

// appendOnce is one attempt of AppendMessage: the whole thing in one
// transaction, returning the changes to emit after commit.
func (s *Store) appendOnce(ctx context.Context, mailboxID string, m Message) (AppendResult, []Change, error) {
	now := s.now().Format(time.RFC3339)
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return AppendResult{}, nil, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	// Lock the mailbox row for the rest of the transaction.
	var uidv uint32
	err = tx.QueryRowContext(ctx, s.rb(`
		SELECT uidvalidity FROM imap_mailboxes WHERE id = ? AND deleted_at IS NULL`+s.dialect.LockClause()),
		mailboxID).Scan(&uidv)
	if errors.Is(err, sql.ErrNoRows) {
		return AppendResult{}, nil, fmt.Errorf("imap: mailbox %s not found", mailboxID)
	}
	if err != nil {
		return AppendResult{}, nil, fmt.Errorf("imap: lock mailbox: %w", err)
	}

	var res AppendResult
	var changes []Change
	if m.ObjectKey != "" {
		var oldUID uint32
		var oldSha string
		err := tx.QueryRowContext(ctx, s.rb(`
			SELECT uid, sha256 FROM imap_messages WHERE mailbox_id = ? AND object_key = ?`),
			mailboxID, m.ObjectKey).Scan(&oldUID, &oldSha)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return AppendResult{}, nil, fmt.Errorf("imap: lookup object_key: %w", err)
		case oldSha == m.SHA256:
			return AppendResult{UID: oldUID, UIDValidity: uidv, Noop: true}, nil, nil
		default:
			var oldSeq uint32
			if err := tx.QueryRowContext(ctx, s.rb(`
				SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ? AND uid <= ?`), mailboxID, oldUID).Scan(&oldSeq); err != nil {
				return AppendResult{}, nil, fmt.Errorf("imap: seq of replaced: %w", err)
			}
			if _, err := tx.ExecContext(ctx, s.rb(`DELETE FROM imap_messages WHERE mailbox_id = ? AND uid = ?`), mailboxID, oldUID); err != nil {
				return AppendResult{}, nil, fmt.Errorf("imap: expunge replaced: %w", err)
			}
			res.Replaced = true
			res.ReplacedUID = oldUID
			changes = append(changes, Change{MailboxID: mailboxID, Kind: ChangeExpunge, UID: oldUID, Seq: oldSeq})
		}
	}

	// Allocate the UID atomically on the mailbox row; uidnext is stored,
	// never MAX(uid)+1, so a removed tail never recycles a UID.
	var uid uint32
	var modseq int64
	if err := tx.QueryRowContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET uidnext = uidnext + 1, modseq = modseq + 1
		 WHERE id = ? AND deleted_at IS NULL
		 RETURNING uidnext - 1, uidvalidity, modseq`), mailboxID).Scan(&uid, &uidv, &modseq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AppendResult{}, nil, fmt.Errorf("imap: mailbox %s not found", mailboxID)
		}
		return AppendResult{}, nil, fmt.Errorf("imap: allocate uid: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rb(`
		INSERT INTO imap_messages
			(mailbox_id, uid, object_key, kind, sha256, format_version, size, internaldate, flags,
			 envelope, bodystructure, subject, from_addr, text_excerpt, parts, modseq, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		mailboxID, uid, m.ObjectKey, m.Kind, m.SHA256, m.FormatVersion, m.Size,
		m.InternalDate.UTC().Format(time.RFC3339), flagsJSON(m.Flags),
		rawOr(m.Envelope, "{}"), rawOr(m.BodyStructure, "{}"), m.Subject, m.FromAddr, m.TextExcerpt,
		rawOr(m.Parts, "[]"), modseq, m.State, now); err != nil {
		if s.dialect.IsUniqueViolationGeneric(err) {
			return AppendResult{}, nil, err // retried by AppendMessage
		}
		return AppendResult{}, nil, fmt.Errorf("imap: insert message: %w", err)
	}
	var total uint32
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ?`), mailboxID).Scan(&total); err != nil {
		return AppendResult{}, nil, fmt.Errorf("imap: count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AppendResult{}, nil, fmt.Errorf("imap: commit: %w", err)
	}
	res.UID = uid
	res.UIDValidity = uidv
	changes = append(changes, Change{MailboxID: mailboxID, Kind: ChangeAppend, UID: uid, Seq: total, Total: total})
	return res, changes, nil
}

// ListMessageHeads returns the mailbox's live messages in UID order — the
// slice a session holds while selected (sequence number = index + 1).
func (s *Store) ListMessageHeads(ctx context.Context, mailboxID string) ([]MessageHead, error) {
	return s.listHeads(ctx, s.db, mailboxID)
}

func (s *Store) listHeads(ctx context.Context, q rowQuerier, mailboxID string) ([]MessageHead, error) {
	rows, err := q.QueryContext(ctx, s.rb(`
		SELECT uid, flags, size, internaldate, modseq FROM imap_messages
		 WHERE mailbox_id = ? ORDER BY uid`), mailboxID)
	if err != nil {
		return nil, fmt.Errorf("imap: list heads: %w", err)
	}
	defer rows.Close()
	var out []MessageHead
	for rows.Next() {
		var h MessageHead
		var flags, idate string
		if err := rows.Scan(&h.UID, &flags, &h.Size, &idate, &h.ModSeq); err != nil {
			return nil, fmt.Errorf("imap: scan head: %w", err)
		}
		_ = json.Unmarshal([]byte(flags), &h.Flags)
		h.InternalDate = parseTime(idate)
		out = append(out, h)
	}
	return out, rows.Err()
}

const messageCols = `mailbox_id, uid, object_key, kind, sha256, format_version, size, internaldate, flags,
	envelope, bodystructure, subject, from_addr, text_excerpt, parts, modseq, state, created_at`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var idate, flags, env, bs, parts, created string
	if err := row.Scan(&m.MailboxID, &m.UID, &m.ObjectKey, &m.Kind, &m.SHA256, &m.FormatVersion, &m.Size,
		&idate, &flags, &env, &bs, &m.Subject, &m.FromAddr, &m.TextExcerpt, &parts, &m.ModSeq, &m.State, &created); err != nil {
		return Message{}, err
	}
	m.InternalDate = parseTime(idate)
	_ = json.Unmarshal([]byte(flags), &m.Flags)
	m.Envelope = json.RawMessage(env)
	m.BodyStructure = json.RawMessage(bs)
	m.Parts = json.RawMessage(parts)
	m.CreatedAt = parseTime(created)
	return m, nil
}

// GetMessage returns one full row.
func (s *Store) GetMessage(ctx context.Context, mailboxID string, uid uint32) (Message, bool, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+messageCols+` FROM imap_messages WHERE mailbox_id = ? AND uid = ?`), mailboxID, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("imap: get message: %w", err)
	}
	return m, true, nil
}

// GetMessageByKey returns the row holding object_key.
func (s *Store) GetMessageByKey(ctx context.Context, mailboxID, objectKey string) (Message, bool, error) {
	if objectKey == "" {
		return Message{}, false, nil
	}
	m, err := scanMessage(s.db.QueryRowContext(ctx, s.rb(`
		SELECT `+messageCols+` FROM imap_messages WHERE mailbox_id = ? AND object_key = ?`), mailboxID, objectKey))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("imap: get message by key: %w", err)
	}
	return m, true, nil
}

// SetFlags replaces a message's flag set and returns the stored
// (normalised) set. Flags are the only mutable state under a UID.
func (s *Store) SetFlags(ctx context.Context, mailboxID string, uid uint32, flags []string) ([]string, error) {
	norm := NormalizeFlags(flags)
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var modseq int64
	if err := tx.QueryRowContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET modseq = modseq + 1 WHERE id = ? RETURNING modseq`), mailboxID).Scan(&modseq); err != nil {
		return nil, fmt.Errorf("imap: bump modseq: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.rb(`
		UPDATE imap_messages SET flags = ?, modseq = ? WHERE mailbox_id = ? AND uid = ?`),
		flagsJSON(norm), modseq, mailboxID, uid)
	if err != nil {
		return nil, fmt.Errorf("imap: set flags: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("imap: message %d not found", uid)
	}
	var seq uint32
	if err := tx.QueryRowContext(ctx, s.rb(`
		SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ? AND uid <= ?`), mailboxID, uid).Scan(&seq); err != nil {
		return nil, fmt.Errorf("imap: seq: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("imap: commit: %w", err)
	}
	s.emit([]Change{{MailboxID: mailboxID, Kind: ChangeFlags, UID: uid, Seq: seq, Flags: norm, Origin: originFrom(ctx)}})
	return norm, nil
}

// RemoveMessage expunges one row (an op-side removal). Returns false when
// no such UID. The mailbox row is bumped (and thereby locked) first so the
// reported sequence number cannot shift under a concurrent writer.
func (s *Store) RemoveMessage(ctx context.Context, mailboxID string, uid uint32) (bool, error) {
	tx, err := s.dialect.BeginWrite(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("imap: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var modseq int64
	if err := tx.QueryRowContext(ctx, s.rb(`
		UPDATE imap_mailboxes SET modseq = modseq + 1 WHERE id = ? RETURNING modseq`), mailboxID).Scan(&modseq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("imap: bump modseq: %w", err)
	}
	var seq uint32
	if err := tx.QueryRowContext(ctx, s.rb(`
		SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ? AND uid <= ?`), mailboxID, uid).Scan(&seq); err != nil {
		return false, fmt.Errorf("imap: seq: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.rb(`DELETE FROM imap_messages WHERE mailbox_id = ? AND uid = ?`), mailboxID, uid)
	if err != nil {
		return false, fmt.Errorf("imap: remove: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // rollback undoes the bump
	}
	var total uint32
	if err := tx.QueryRowContext(ctx, s.rb(`SELECT COUNT(*) FROM imap_messages WHERE mailbox_id = ?`), mailboxID).Scan(&total); err != nil {
		return false, fmt.Errorf("imap: count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("imap: commit: %w", err)
	}
	s.emit([]Change{{MailboxID: mailboxID, Kind: ChangeExpunge, UID: uid, Seq: seq, Total: total, Origin: originFrom(ctx)}})
	return true, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
