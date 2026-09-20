package authn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Only the stack that created a principal administers it.
//
// Every users, principal_bindings and credentials row records the stack that
// wrote it (created_by). A principal's rows all come from ONE stack — the
// one that wrote its first row — and a write from any other stack is
// refused. A person is therefore one user across the tenant without any
// stack in that tenant being able to reset their password by accident.
//
// What this guards against is MISTAKES, not a hostile author. Stacks in a
// tenant share one deploy authority (every admin stack check is
// `opstack:*:…`), so whoever can change one stack can change them all, and a
// fully qualified `_txc.goto` into the owning stack is a documented feature:
// the owner's rules then run and make the call, which this rule allows.
// Revisit if deploy authority is ever granted per stack.
//
// The caller's stack comes from the dispatching rule (processor.StackScope),
// never from the envelope.

// BaseStack reduces a dispatching stack name to the name that owns rows: its
// first slash segment. `web`, its canary slot `web/canary` and its mail
// channel `web/_mail` are all the stack `web` — a slot is a sparse override
// of its base (processor.OpsForStage), so if it did not count as the base a
// canary could never manage the users its stable slot created.
func BaseStack(stack string) string {
	stack = strings.TrimLeft(strings.TrimSpace(stack), "/")
	if i := strings.IndexByte(stack, '/'); i >= 0 {
		return stack[:i]
	}
	return stack
}

// OwnerError is the ErrNotOwner a refused write returns: it names the stack
// that does own the principal, so the author knows where to `_txc.goto`.
type OwnerError struct {
	Principal Principal
	Owner     string
}

func (e *OwnerError) Error() string {
	return fmt.Sprintf("authn: %s is managed by stack %q", e.Principal.ID, e.Owner)
}

func (e *OwnerError) Is(target error) bool { return target == ErrNotOwner }

// ownerOf returns the stack that owns p's rows, or found=false when p has no
// rows yet (the next writer becomes its owner). A `user` principal is owned
// by its users row and must have one: ErrNotFound otherwise.
//
// Revoked rows count. Ownership is sticky — revoking a principal's last
// credential does not put it up for grabs.
//
// Two stacks racing to write the FIRST row of the same brand-new non-user
// principal can both pass this check. The earliest row then decides (the
// ORDER BY), so the loser's later writes are refused; nothing is forged, and
// the stacks involved share a deploy authority anyway.
func (s *Store) ownerOf(ctx context.Context, q querier, tenantID string, p Principal) (owner string, found bool, err error) {
	if uid, isUser := p.UserID(); isUser {
		err = s.qr(ctx, q, `SELECT created_by FROM users WHERE id = ? AND tenant_id = ?`, uid, tenantID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return owner, err == nil, err
	}
	err = s.qr(ctx, q, `
		SELECT created_by FROM (
			SELECT created_by, created_at, id FROM principal_bindings WHERE tenant_id = ? AND principal_id = ?
			UNION ALL
			SELECT created_by, created_at, id FROM credentials WHERE tenant_id = ? AND principal_id = ?
		) AS r ORDER BY created_at, id LIMIT 1`,
		tenantID, p.ID, tenantID, p.ID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return owner, err == nil, err
}

// authorize is the gate every write passes: it returns the base stack to
// stamp as created_by, or ErrNoStack / *OwnerError / ErrNotFound.
func (s *Store) authorize(ctx context.Context, q querier, tenantID, stack string, p Principal) (base string, err error) {
	base = BaseStack(stack)
	if base == "" {
		return "", ErrNoStack
	}
	owner, found, err := s.ownerOf(ctx, q, tenantID, p)
	if err != nil {
		return "", err
	}
	if found && owner != base {
		return "", &OwnerError{Principal: p, Owner: owner}
	}
	return base, nil
}

// RequireOwner reports whether stack manages p, for an op that grants p
// something the identity tables do not hold (a printer, say). Unlike the
// writes above it does not make stack the owner of a principal nobody has
// written yet: p must already exist — ErrNotFound otherwise — so a grant
// cannot name a principal another stack could later claim. Another stack's
// principal is an *OwnerError. It returns the base stack.
func (s *Store) RequireOwner(ctx context.Context, tenantID, stack string, p Principal) (base string, err error) {
	if err := requireTenant(tenantID); err != nil {
		return "", err
	}
	base = BaseStack(stack)
	if base == "" {
		return "", ErrNoStack
	}
	owner, found, err := s.ownerOf(ctx, s.DB, tenantID, p)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "", ErrNotFound
	case owner != base:
		return "", &OwnerError{Principal: p, Owner: owner}
	}
	return base, nil
}
