package authn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// Store reads and writes the three identity tables. It owns their layout:
// the ops (chassis/server) and, later, the protocol account ops and the
// login resolver go through these methods and never see a column.
//
// The tables live in auth.db (SQLite by default, the shared Postgres on a
// fleet), created by numbered migration 0005 — the Store ships no DDL.
type Store struct {
	DB      *sql.DB
	Dialect registry.Dialect

	now func() time.Time
}

// NewStore builds a Store over an open, migrated auth DB. A nil dialect is
// SQLite.
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{DB: db, Dialect: d, now: time.Now}
}

// SetClock replaces the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

var (
	// ErrNotFound: no such user, binding or credential in this tenant.
	ErrNotFound = errors.New("authn: not found")
	// ErrInvalid wraps every argument error; the message says what to fix.
	ErrInvalid = errors.New("authn: invalid argument")
	// ErrNotOwner: the principal's rows were written by another stack. The
	// returned error is an *OwnerError naming it.
	ErrNotOwner = errors.New("authn: principal is managed by another stack")
	// ErrNoStack: a write arrived with no dispatching stack to attribute it to.
	ErrNoStack = errors.New("authn: no stack in request scope")
	// ErrBound: the identifier is already bound to a different principal.
	ErrBound = errors.New("authn: identifier is bound to another principal")
	// ErrDisabled: the user is disabled, so nothing new may be issued to it.
	ErrDisabled = errors.New("authn: user is disabled")
	// ErrTooMany: the principal already holds MaxCredentials live credentials.
	ErrTooMany = errors.New("authn: too many credentials")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// querier is the read surface *sql.DB and *sql.Tx share.
type querier interface {
	ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row
}

func (s *Store) ex(ctx context.Context, q querier, query string, a ...any) (sql.Result, error) {
	return q.ExecContext(ctx, s.Dialect.Rebind(query), a...)
}
func (s *Store) qy(ctx context.Context, q querier, query string, a ...any) (*sql.Rows, error) {
	return q.QueryContext(ctx, s.Dialect.Rebind(query), a...)
}
func (s *Store) qr(ctx context.Context, q querier, query string, a ...any) *sql.Row {
	return q.QueryRowContext(ctx, s.Dialect.Rebind(query), a...)
}

// stamp is the column form of a time: RFC3339 UTC at second resolution, so
// the strings sort chronologically (RFC3339Nano trims zeros and would not).
func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339) }

func parseStamp(v string) time.Time {
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

func parseStampPtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseStamp(v.String)
	return &t
}

func requireTenant(tenantID string) error {
	if strings.TrimSpace(tenantID) == "" {
		return invalid("no tenant")
	}
	return nil
}

// cleanText trims s and refuses control characters and anything past max
// runes — for the free-text columns (display name, label) that end up in an
// admin UI and a log line.
func cleanText(field, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", invalid("%s may not hold control characters", field)
		}
		n++
	}
	if n > max {
		return "", invalid("%s is at most %d characters", field, max)
	}
	return s, nil
}
