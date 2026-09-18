package authn

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// User statuses.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// User mirrors a users row: durable state about a human. A pony or a service
// is a principal without one.
type User struct {
	ID          string // usr_<hxid>
	TenantID    string
	DisplayName string
	Status      string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Principal is the principal this user authenticates and is authorized as.
func (u User) Principal() Principal { return UserPrincipal(u.ID) }

// Disabled reports whether the user may no longer sign in or be issued
// credentials.
func (u User) Disabled() bool { return u.Status != StatusActive }

// NewUser is what txco://user/create supplies.
type NewUser struct {
	// Email is required: it is the identifier the user is found by, bound as
	// a BindEmail binding in the same transaction.
	Email string
	// EmailVerified: the product proved the address (see NewBinding.Verified).
	EmailVerified bool
	DisplayName   string
}

const maxDisplayName = 256

const userCols = `id, tenant_id, display_name, status, created_by, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, updated string
	err := row.Scan(&u.ID, &u.TenantID, &u.DisplayName, &u.Status, &u.CreatedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.CreatedAt, u.UpdatedAt = parseStamp(created), parseStamp(updated)
	return u, nil
}

// GetUser reads a user by id. Reads are tenant-wide: any stack may look a
// user up (and learn from CreatedBy which stack manages it).
func (s *Store) GetUser(ctx context.Context, tenantID, userID string) (User, error) {
	if err := requireTenant(tenantID); err != nil {
		return User{}, err
	}
	return s.getUser(ctx, s.DB, tenantID, userID)
}

func (s *Store) getUser(ctx context.Context, q querier, tenantID, userID string) (User, error) {
	return scanUser(s.qr(ctx, q,
		`SELECT `+userCols+` FROM users WHERE id = ? AND tenant_id = ?`, userID, tenantID))
}

// UserByEmail resolves a live email binding to its user. ErrNotFound when the
// address is unbound, or bound to a principal that is not a user.
func (s *Store) UserByEmail(ctx context.Context, tenantID, email string) (User, Binding, error) {
	b, err := s.LookupBinding(ctx, tenantID, BindEmail, "", email)
	if err != nil {
		return User{}, Binding{}, err
	}
	uid, isUser := b.Principal.UserID()
	if !isUser {
		return User{}, Binding{}, ErrNotFound
	}
	u, err := s.getUser(ctx, s.DB, tenantID, uid)
	return u, b, err
}

// CreateUser creates a user and binds its email, atomically, on behalf of
// stack. It is idempotent on the email: creating a user whose address is
// already bound to a user THIS stack manages returns that user with
// created=false (and upgrades the binding to verified if asked), so a
// retried pipeline does not fail or fork the person in two. The display name
// is not touched on that path.
//
// An address bound to another stack's user is ErrNotOwner; one bound to a
// non-user principal (a pony's mailbox) is ErrBound.
func (s *Store) CreateUser(ctx context.Context, tenantID, stack string, in NewUser) (User, Binding, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return User{}, Binding{}, false, err
	}
	base := BaseStack(stack)
	if base == "" {
		return User{}, Binding{}, false, ErrNoStack
	}
	nb, err := NewBinding{Kind: BindEmail, Subject: in.Email, Verified: in.EmailVerified}.normalize()
	if err != nil {
		return User{}, Binding{}, false, err
	}
	name, err := cleanText("display_name", in.DisplayName, maxDisplayName)
	if err != nil {
		return User{}, Binding{}, false, err
	}

	// Two passes, as in BindPrincipal: a lost race on the binding's unique
	// index rolls the whole insert back, and the second pass finds the winner.
	for attempt := 0; ; attempt++ {
		existing, err := s.lookupBinding(ctx, s.DB, tenantID, nb)
		switch {
		case err == nil:
			u, b, err := s.existingUser(ctx, tenantID, base, existing, nb.Verified)
			return u, b, false, err
		case !errors.Is(err, ErrNotFound):
			return User{}, Binding{}, false, err
		}
		u, b, err := s.insertUser(ctx, tenantID, base, name, nb)
		if err == nil {
			return u, b, true, nil
		}
		if attempt > 0 || !s.Dialect.IsUniqueViolationGeneric(err) {
			return User{}, Binding{}, false, err
		}
	}
}

// existingUser is CreateUser's idempotent path: the address is already bound.
func (s *Store) existingUser(ctx context.Context, tenantID, base string, b Binding, verified bool) (User, Binding, error) {
	uid, isUser := b.Principal.UserID()
	if !isUser {
		return User{}, Binding{}, ErrBound
	}
	u, err := s.getUser(ctx, s.DB, tenantID, uid)
	if err != nil {
		return User{}, Binding{}, err
	}
	if u.CreatedBy != base {
		return User{}, Binding{}, &OwnerError{Principal: b.Principal, Owner: u.CreatedBy}
	}
	b, err = s.upgradeVerified(ctx, s.DB, b, verified)
	return u, b, err
}

func (s *Store) insertUser(ctx context.Context, tenantID, base, name string, nb NewBinding) (User, Binding, error) {
	tx, err := s.Dialect.BeginWrite(ctx, s.DB)
	if err != nil {
		return User{}, Binding{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.stamp()
	u := User{
		ID: "usr_" + hxid.NewTimeSort().String(), TenantID: tenantID, DisplayName: name,
		Status: StatusActive, CreatedBy: base, CreatedAt: parseStamp(now), UpdatedAt: parseStamp(now),
	}
	if _, err := s.ex(ctx, tx,
		`INSERT INTO users (id, tenant_id, display_name, status, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, tenantID, name, StatusActive, base, now, now); err != nil {
		return User{}, Binding{}, err
	}
	b, err := s.insertBinding(ctx, tx, tenantID, base, u.Principal(), nb)
	if err != nil {
		return User{}, Binding{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, Binding{}, err
	}
	return u, b, nil
}

// SetUserDisabled disables (or re-enables) a user on behalf of stack. A
// disabled user keeps its bindings and credentials — nothing is revoked, so
// re-enabling restores access — but may not be issued new credentials, and
// the login resolver refuses it.
func (s *Store) SetUserDisabled(ctx context.Context, tenantID, stack, userID string, disabled bool) (User, error) {
	if err := requireTenant(tenantID); err != nil {
		return User{}, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, UserPrincipal(userID)); err != nil {
		return User{}, err
	}
	status := StatusActive
	if disabled {
		status = StatusDisabled
	}
	if _, err := s.ex(ctx, s.DB,
		`UPDATE users SET status = ?, updated_at = ? WHERE id = ? AND tenant_id = ? AND status <> ?`,
		status, s.stamp(), userID, tenantID, status); err != nil {
		return User{}, err
	}
	return s.getUser(ctx, s.DB, tenantID, userID)
}
