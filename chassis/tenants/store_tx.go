package tenants

// Tx-accepting variants of the Store mutation methods. Callers that
// need to bundle a tenants/hostnames mutation with other writes in
// the same SQLite transaction (e.g. the admin fleet-sync producer
// hooks that append a control_events_outbox row alongside) use these
// instead of the s.DB-direct variants above.
//
// The non-Tx methods above remain the canonical surface for callers
// that don't need atomicity with sibling writes (the CLI, tests,
// non-admin flows). Each Tx variant delegates to the same SQL —
// the only difference is the execer.

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

// execer is implemented by both *sql.DB and *sql.Tx. The shared SQL
// helpers below take this interface so the same body services both
// the bare-DB and inside-tx call sites.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// CreateTx inserts a tenants row inside the caller's tx. Same
// validation and SQL as Create. Returns the same errors.
func (s *Store) CreateTx(ctx context.Context, tx *sql.Tx, t Tenant) error {
	return createTenant(ctx, tx, t, s.dia())
}

func createTenant(ctx context.Context, x execer, t Tenant, d registry.Dialect) error {
	if t.TenantID == "" {
		return errors.New("tenants: empty tenant_id")
	}
	slug := strings.ToLower(strings.TrimSpace(t.Slug))
	if slug == "" {
		return errors.New("tenants: empty tenant slug")
	}
	if ReservedSlug(slug) {
		return fmt.Errorf("tenants: slug %q is reserved (the _ prefix is chassis-internal)", slug)
	}
	now := t.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var nameArg any
	if t.Name == "" {
		nameArg = nil
	} else {
		nameArg = t.Name
	}
	_, err := x.ExecContext(ctx,
		orSQLite(d).Rebind(`INSERT INTO tenants (tenant_id, slug, name, created_at)
		 VALUES (?, ?, ?, ?)`),
		t.TenantID, slug, nameArg, now.UTC().Format(time.RFC3339))
	return err
}

// CreateHostnameTx is the tx-accepting variant of CreateHostname.
// Differs in one important way: the caller MUST pre-generate the
// hostname ID and pass it in via h.ID. (Generating inside the tx
// would prevent the producer hook from including the ID in its
// pre-tx artifact JSON.) An empty h.ID returns an error.
//
// On unique-violation the caller has to do their own follow-up
// lookup — we can't run LookupActiveHostname inside the same tx
// without an explicit second query, and the original CreateHostname
// path's "lookup the conflicting owner" affordance isn't useful
// when the caller is the producer hook (which is publishing
// authoritative state, not reconciling with an existing row).
func (s *Store) CreateHostnameTx(ctx context.Context, tx *sql.Tx, h Hostname) error {
	if h.ID == "" {
		return errors.New("tenants: CreateHostnameTx requires caller-supplied h.ID")
	}
	canon, ok := CanonicalizeHost(h.Hostname)
	if !ok || !IsValidHostname(canon) {
		return errors.New("tenants: invalid hostname")
	}
	if h.TenantID == "" {
		return errors.New("tenants: empty tenant_id")
	}
	now := h.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var createdByArg any
	if h.CreatedBy != "" {
		createdByArg = h.CreatedBy
	}
	// verified_at is OPTIONAL on insert: a non-nil VerifiedAt is the
	// auto-verify signal (admin handler stamps it for dev-local
	// hostnames). Most callers leave it nil, the normal proof-of-
	// ownership flow stamps verified_at later via the UPDATE at
	// line 175.
	var verifiedAtArg any
	if h.VerifiedAt != nil {
		verifiedAtArg = h.VerifiedAt.UTC().Format(time.RFC3339)
	}
	_, err := tx.ExecContext(ctx,
		s.rb(`INSERT INTO tenant_hostnames
		     (id, hostname, tenant_id, stack, created_at, created_by, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		h.ID, canon, h.TenantID, h.Stack,
		now.Format(time.RFC3339), createdByArg, verifiedAtArg)
	if err != nil {
		if s.dia().IsUniqueViolationGeneric(err) {
			return ErrHostnameInUse
		}
		return err
	}
	return nil
}

// NewHostnameID generates the canonical thn_ prefixed id callers pass
// into CreateHostnameTx. Lives here so the producer hook + the Store
// agree on the format.
func NewHostnameID() string {
	return "thn_" + hxid.New().String()
}

// AttachHostnameTx is the tx-accepting variant of AttachHostname.
// Returns ErrNotFound when no active row matches the canonical
// hostname.
func (s *Store) AttachHostnameTx(ctx context.Context, tx *sql.Tx, hostname, stack string) error {
	canon, ok := CanonicalizeHost(hostname)
	if !ok {
		return ErrNotFound
	}
	if stack == "" {
		return errors.New("tenants: empty stack")
	}
	res, err := tx.ExecContext(ctx,
		s.rb(`UPDATE tenant_hostnames
		    SET stack = ?
		  WHERE hostname = ? AND revoked_at IS NULL`),
		stack, canon)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeHostnameTx is the tx-accepting variant of RevokeHostname.
// Idempotent; reports the canonical hostname and the timestamp it
// stamped so the caller can build a consistent fleet-sync artifact
// even when the underlying row was already revoked (in which case
// no rows are affected but the timestamp returned is when the call
// was made — the producer's view of the moment).
func (s *Store) RevokeHostnameTx(ctx context.Context, tx *sql.Tx, hostname string) (canonical string, revokedAt time.Time, err error) {
	canon, ok := CanonicalizeHost(hostname)
	if !ok {
		return "", time.Time{}, nil // lenient
	}
	now := time.Now().UTC()
	_, execErr := tx.ExecContext(ctx,
		s.rb(`UPDATE tenant_hostnames
		    SET revoked_at = ?
		  WHERE hostname = ? AND revoked_at IS NULL`),
		now.Format(time.RFC3339), canon)
	if execErr != nil {
		return canon, now, execErr
	}
	return canon, now, nil
}

// MarkHostnameVerifiedTx is the tx-accepting variant of
// MarkHostnameVerified.
func (s *Store) MarkHostnameVerifiedTx(ctx context.Context, tx *sql.Tx, hostnameID string, when time.Time) error {
	_, err := tx.ExecContext(ctx,
		s.rb(`UPDATE tenant_hostnames SET verified_at = ? WHERE id = ?`),
		when.UTC().Format(time.RFC3339), hostnameID)
	return err
}
