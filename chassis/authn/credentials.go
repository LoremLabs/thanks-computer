package authn

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// CredentialKind is the kind of secret a credential holds.
type CredentialKind string

// KindAppPassword is a chassis-issued password (see password.go), stored as
// an argon2id PHC hash. The only kind built.
const KindAppPassword CredentialKind = "app_password"

// MaxCredentials bounds the LIVE credentials one principal may hold — far
// past "one per device", and enough to stop a loop from minting argon2
// hashes without end.
const MaxCredentials = 64

const maxLabel = 128

// Credential mirrors a credentials row, minus the hash: nothing outside this
// package ever holds one.
//
// It records only the principal it authenticates AS. Who holds it, or on
// whose behalf, is deliberately not modeled: when a pony's password is typed
// by its owner, the pony is the principal.
type Credential struct {
	ID         string // crd_<hxid>
	TenantID   string
	Principal  Principal
	ShortID    string
	Kind       CredentialKind
	Scopes     Scopes
	Label      string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the credential can no longer authenticate.
func (c Credential) Revoked() bool { return c.RevokedAt != nil }

// NewCredential is what txco://credential/create supplies.
type NewCredential struct {
	Scopes []string
	Label  string
	Style  PasswordStyle // "" ⇒ StyleToken
	Words  int           // StyleWords only; 0 ⇒ the default
}

const credentialCols = `id, tenant_id, principal_id, short_id, kind, scopes, label,
	created_by, created_at, last_used_at, revoked_at`

func scanCredential(row interface{ Scan(...any) error }, extra ...any) (Credential, error) {
	var (
		c             Credential
		pid, scopes   string
		created       string
		used, revoked sql.NullString
	)
	dest := append([]any{&c.ID, &c.TenantID, &pid, &c.ShortID, &c.Kind, &scopes, &c.Label,
		&c.CreatedBy, &created, &used, &revoked}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	kind, _, _ := strings.Cut(pid, ":")
	c.Principal = Principal{ID: pid, Kind: PrincipalKind(kind)}
	if c.Scopes, err = decodeScopes(scopes); err != nil {
		return Credential{}, err
	}
	c.CreatedAt = parseStamp(created)
	c.LastUsedAt, c.RevokedAt = parseStampPtr(used), parseStampPtr(revoked)
	return c, nil
}

// IssueCredential mints a credential that authenticates as p, on behalf of
// stack, and returns its password — the only time the cleartext exists
// outside the caller. Only the argon2id hash is stored.
//
// A principal with no rows yet becomes this stack's: issuing its first
// credential is what claims it. A `user` principal must exist and be active.
func (s *Store) IssueCredential(ctx context.Context, tenantID, stack string, p Principal, in NewCredential) (Credential, string, error) {
	if err := requireTenant(tenantID); err != nil {
		return Credential{}, "", err
	}
	scopes, err := ParseScopes(in.Scopes)
	if err != nil {
		return Credential{}, "", invalid("%v", err)
	}
	label, err := cleanText("label", in.Label, maxLabel)
	if err != nil {
		return Credential{}, "", err
	}
	if _, err := issuePassword(in.Style, in.Words, "xxxx"); err != nil { // validate before any I/O
		return Credential{}, "", invalid("%v", err)
	}
	base, err := s.authorize(ctx, s.DB, tenantID, stack, p)
	if err != nil {
		return Credential{}, "", err
	}
	if uid, isUser := p.UserID(); isUser {
		u, err := s.getUser(ctx, s.DB, tenantID, uid)
		if err != nil {
			return Credential{}, "", err
		}
		if u.Disabled() {
			return Credential{}, "", ErrDisabled
		}
	}
	var live int
	if err := s.qr(ctx, s.DB,
		`SELECT COUNT(*) FROM credentials WHERE tenant_id = ? AND principal_id = ? AND revoked_at IS NULL`,
		tenantID, p.ID).Scan(&live); err != nil {
		return Credential{}, "", err
	}
	if live >= MaxCredentials {
		return Credential{}, "", ErrTooMany
	}

	// A short-id collision within the principal (≈ live/531k) trips the
	// UNIQUE index; re-roll. The hash covers the whole password, id included,
	// so each attempt hashes again — rare enough not to matter.
	for attempt := 0; ; attempt++ {
		c := Credential{
			ID: "crd_" + hxid.NewTimeSort().String(), TenantID: tenantID, Principal: p,
			ShortID: newShortID(), Kind: KindAppPassword, Scopes: scopes, Label: label, CreatedBy: base,
		}
		password, err := issuePassword(in.Style, in.Words, c.ShortID)
		if err != nil {
			return Credential{}, "", invalid("%v", err)
		}
		hash, err := apppass.HashPassword(password)
		if err != nil {
			return Credential{}, "", err
		}
		created := s.stamp()
		c.CreatedAt = parseStamp(created)
		_, err = s.ex(ctx, s.DB,
			`INSERT INTO credentials
			   (id, tenant_id, principal_id, short_id, kind, secret_hash, scopes, label, created_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, tenantID, p.ID, c.ShortID, string(c.Kind), hash, scopes.encode(), label, base, created)
		if err == nil {
			return c, password, nil
		}
		if attempt >= 4 || !s.Dialect.IsUniqueViolationGeneric(err) {
			return Credential{}, "", err
		}
	}
}

// ListCredentials returns p's credentials, newest first, on behalf of stack —
// the owner only: labels, scopes and last-used times are for whoever manages
// the principal. Revoked rows are included when asked.
func (s *Store) ListCredentials(ctx context.Context, tenantID, stack string, p Principal, includeRevoked bool) ([]Credential, error) {
	if err := requireTenant(tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return nil, err
	}
	q := `SELECT ` + credentialCols + ` FROM credentials WHERE tenant_id = ? AND principal_id = ?`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	rows, err := s.qy(ctx, s.DB, q+` ORDER BY created_at DESC, id DESC`, tenantID, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeCredential revokes one credential by id, on behalf of stack. It
// returns the row and whether THIS call revoked it (false: it already was).
// Revocation is permanent; the row stays, so its short id is never reused.
func (s *Store) RevokeCredential(ctx context.Context, tenantID, stack, credentialID string) (Credential, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Credential{}, false, err
	}
	c, err := scanCredential(s.qr(ctx, s.DB,
		`SELECT `+credentialCols+` FROM credentials WHERE id = ? AND tenant_id = ?`, credentialID, tenantID))
	if err != nil {
		return Credential{}, false, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, c.Principal); err != nil {
		return Credential{}, false, err
	}
	if c.Revoked() {
		return c, false, nil
	}
	now := s.stamp()
	res, err := s.ex(ctx, s.DB,
		`UPDATE credentials SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		now, credentialID, tenantID)
	if err != nil {
		return Credential{}, false, err
	}
	n, _ := res.RowsAffected()
	t := parseStamp(now)
	c.RevokedAt = &t
	return c, n > 0, nil
}

// RevokeCredentialOf is RevokeCredential for a caller that knows WHOSE the
// credential should be: it revokes credentialID only if it belongs to p, and
// answers ErrNotFound otherwise — the same answer as an id that does not
// exist, so a caller learns nothing about another principal's credentials.
//
// It exists because the creator-stack rule is not a wall between a stack's
// own principals: one stack may manage thousands (every pony of a product),
// and a bare id taken from a request would let one of that stack's users
// revoke a credential of another's. Naming the principal pins the id to it.
func (s *Store) RevokeCredentialOf(ctx context.Context, tenantID, stack string, p Principal, credentialID string) (Credential, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Credential{}, false, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return Credential{}, false, err
	}
	var owner string
	err := s.qr(ctx, s.DB, `SELECT principal_id FROM credentials WHERE id = ? AND tenant_id = ?`, credentialID, tenantID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != p.ID) {
		return Credential{}, false, ErrNotFound
	}
	if err != nil {
		return Credential{}, false, err
	}
	return s.RevokeCredential(ctx, tenantID, stack, credentialID)
}

// RevokeCredentials revokes every live credential of p except the one named,
// and returns how many it revoked. It is the second half of a rotation: issue
// the new credential, then revoke the rest.
//
// exceptID must name a LIVE credential of p (ErrNotFound otherwise, and
// nothing is revoked). That is the point of it: in a pipeline the id comes
// from the issue step, and if that step failed the id is missing — revoking
// "all but nothing" would then lock the principal out with no password left.
// Pass exceptID == "" only to mean revoke ALL, deliberately.
func (s *Store) RevokeCredentials(ctx context.Context, tenantID, stack string, p Principal, exceptID string) (int, error) {
	if err := requireTenant(tenantID); err != nil {
		return 0, err
	}
	if _, err := s.authorize(ctx, s.DB, tenantID, stack, p); err != nil {
		return 0, err
	}
	if exceptID != "" {
		var one int
		err := s.qr(ctx, s.DB,
			`SELECT 1 FROM credentials WHERE id = ? AND tenant_id = ? AND principal_id = ? AND revoked_at IS NULL`,
			exceptID, tenantID, p.ID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		if err != nil {
			return 0, err
		}
	}
	res, err := s.ex(ctx, s.DB,
		`UPDATE credentials SET revoked_at = ?
		 WHERE tenant_id = ? AND principal_id = ? AND revoked_at IS NULL AND id <> ?`,
		s.stamp(), tenantID, p.ID, exceptID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// VerifyPassword checks a presented password against p's credentials: the
// short id in the password picks the row, then ONE argon2 verify decides.
// It returns the credential so the caller can check its scopes cover the
// head being opened.
//
// Every miss — no id in the password, no such credential, a revoked one —
// burns the same dummy verify, so a wrong id costs what a wrong secret does.
//
// This is the store's half of a login. The resolver on top of it (caching,
// throttling, the user's status, last_used_at) is the authentication runtime.
func (s *Store) VerifyPassword(ctx context.Context, tenantID string, p Principal, password string) (Credential, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Credential{}, false, err
	}
	shortID, ok := ShortIDOf(password)
	if !ok {
		apppass.VerifyDummy(password)
		return Credential{}, false, nil
	}
	var hash string
	c, err := scanCredential(s.qr(ctx, s.DB,
		`SELECT `+credentialCols+`, secret_hash FROM credentials
		 WHERE tenant_id = ? AND principal_id = ? AND short_id = ? AND revoked_at IS NULL`,
		tenantID, p.ID, shortID), &hash)
	if errors.Is(err, ErrNotFound) {
		apppass.VerifyDummy(password)
		return Credential{}, false, nil
	}
	if err != nil {
		return Credential{}, false, err
	}
	match, err := apppass.VerifyPassword(hash, password)
	if err != nil || !match {
		return Credential{}, false, err
	}
	return c, true, nil
}
