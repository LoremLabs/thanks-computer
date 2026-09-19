package drive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Account is a drive_accounts row: a Basic-auth login for the `webdav`
// head, bound to exactly ONE collection (the DAV root the login sees). The
// collection is the principal's grant; a credential's `drive:<id>:…` scope
// can only narrow it.
type Account struct {
	Tenant       string
	Username     string
	Status       string
	CollectionID string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NormalizeUsername lowercases and trims a login / op username.
func NormalizeUsername(u string) string {
	return strings.ToLower(strings.TrimSpace(u))
}

// UpsertAccount creates the account or updates it. An empty status /
// collectionID leaves the stored value unchanged on update; collectionID is
// required on create. created reports whether the row was new.
// The account holds no password: who may sign in as it is the identity
// store's business (chassis/authn — a binding and a credential).
func (s *Store) UpsertAccount(ctx context.Context, tenant, username, status, collectionID string) (created bool, err error) {
	username = NormalizeUsername(username)
	if tenant == "" || username == "" {
		return false, errors.New("drive: empty tenant or username")
	}
	if status != "" && status != StatusActive && status != StatusDisabled {
		return false, fmt.Errorf("drive: status must be %s or %s", StatusActive, StatusDisabled)
	}
	if collectionID != "" {
		c, ok, err := s.GetCollectionByID(ctx, collectionID)
		if err != nil {
			return false, err
		}
		if !ok || c.Tenant != tenant {
			return false, fmt.Errorf("%w: collection %s", ErrNotFound, collectionID)
		}
	}
	now := fmtTime(s.now())

	var owner string
	err = s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM drive_accounts WHERE username = ?`), username).Scan(&owner)
	exists := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if collectionID == "" {
			return false, errors.New("drive: a new account needs a collection")
		}
		if status == "" {
			status = StatusActive
		}
		_, ierr := s.db.ExecContext(ctx, s.rb(`
			INSERT INTO drive_accounts (tenant, username, status, collection_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`),
			tenant, username, status, collectionID, now, now)
		switch {
		case ierr == nil:
			return true, nil
		case s.dialect.IsUniqueViolationGeneric(ierr):
			// A concurrent creator won. Re-read: ours ⇒ update; theirs ⇒ taken.
			if rerr := s.db.QueryRowContext(ctx, s.rb(`SELECT tenant FROM drive_accounts WHERE username = ?`), username).Scan(&owner); rerr != nil {
				return false, fmt.Errorf("drive: insert account: %w", ierr)
			}
			if owner != tenant {
				return false, ErrUsernameTaken
			}
			exists = true
		default:
			return false, fmt.Errorf("drive: insert account: %w", ierr)
		}
	case err != nil:
		return false, fmt.Errorf("drive: lookup account: %w", err)
	}
	if exists {
		if owner != tenant {
			return false, ErrUsernameTaken
		}
		sets := []string{"updated_at = ?"}
		args := []any{now}
		if status != "" {
			sets = append(sets, "status = ?")
			args = append(args, status)
		}
		if collectionID != "" {
			sets = append(sets, "collection_id = ?")
			args = append(args, collectionID)
		}
		args = append(args, tenant, username)
		if _, err := s.db.ExecContext(ctx, s.rb(
			`UPDATE drive_accounts SET `+strings.Join(sets, ", ")+` WHERE tenant = ? AND username = ?`), args...); err != nil {
			return false, fmt.Errorf("drive: update account: %w", err)
		}
	}
	return false, nil
}

// GetAccount looks an account up by username (the login identity).
func (s *Store) GetAccount(ctx context.Context, username string) (Account, bool, error) {
	username = NormalizeUsername(username)
	var a Account
	var created, updated string
	err := s.db.QueryRowContext(ctx, s.rb(`
		SELECT tenant, username, status, collection_id, created_at, updated_at
		  FROM drive_accounts WHERE username = ?`), username).
		Scan(&a.Tenant, &a.Username, &a.Status, &a.CollectionID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("drive: get account: %w", err)
	}
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return a, true, nil
}
