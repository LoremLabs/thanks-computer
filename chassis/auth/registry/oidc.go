package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrSubjectAlreadyMapped is returned by CreateOIDCSubject when an
// (issuer, subject) row already exists. The cloud-enroll handler treats
// this as a benign first-enroll race: it re-reads the mapping and
// proceeds as a returning user rather than minting a second tenant.
var ErrSubjectAlreadyMapped = errors.New("oidc subject already mapped")

// LookupOIDCSubject resolves an OIDC (issuer, subject) to the tenant_id
// minted on that identity's first cloud enrollment. Returns ErrNotFound
// when the identity has never enrolled (the caller then runs the
// first-enroll / slug-selection path).
func (r *Registry) LookupOIDCSubject(ctx context.Context, issuer, subject string) (string, error) {
	row := r.qr(ctx, r.DB,
		`SELECT tenant_id FROM oidc_subjects WHERE issuer = ? AND subject = ?`,
		issuer, subject)
	var tenantID string
	switch err := row.Scan(&tenantID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", err
	}
	return tenantID, nil
}

// CreateOIDCSubject records the (issuer, subject) → tenant_id mapping. An
// existing mapping is surfaced as the typed ErrSubjectAlreadyMapped so callers
// can recover from a concurrent first-enroll without minting a duplicate
// tenant. A pre-check covers the common case cleanly across dialects (the
// dialect's IsUniqueViolation is purpose-built for actor_keys, not this table);
// a generic post-insert check catches the pre-check→insert race.
func (r *Registry) CreateOIDCSubject(ctx context.Context, issuer, subject, tenantID string) error {
	if _, err := r.LookupOIDCSubject(ctx, issuer, subject); err == nil {
		return ErrSubjectAlreadyMapped
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err := r.ex(ctx, r.DB,
		`INSERT INTO oidc_subjects (issuer, subject, tenant_id, created_at)
		 VALUES (?, ?, ?, ?)`,
		issuer, subject, tenantID, time.Now().UTC().Format(time.RFC3339))
	if err != nil && isUniqueOIDCViolation(err) {
		return ErrSubjectAlreadyMapped
	}
	return err
}

// isUniqueOIDCViolation detects a UNIQUE/PK violation on oidc_subjects across
// SQLite and Postgres. The registry Dialect's IsUniqueViolation is scoped to
// actor_keys.public_key, so this generic check serves the oidc_subjects race.
func isUniqueOIDCViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") || // sqlite
		strings.Contains(msg, "23505") || // postgres SQLSTATE unique_violation
		strings.Contains(msg, "duplicate key value") // postgres message text
}
