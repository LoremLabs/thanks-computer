package authn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// BindingKind is the kind of identifier a binding maps to a principal.
type BindingKind string

const (
	// BindEmail binds an address. issuer is empty; subject is the lowercased
	// address. A login's username resolves through this kind.
	BindEmail BindingKind = "email"
	// BindOIDC binds an (issuer, subject) pair from an identity provider.
	// The store keeps it; nothing consumes it yet (OIDC is its own project).
	BindOIDC BindingKind = "oidc"
)

// reservedBindingKinds are named in the schema so the abstraction is known
// not to assume OIDC, but nothing builds them yet.
var reservedBindingKinds = map[BindingKind]bool{"did": true, "atproto_handle": true}

// Binding mirrors a principal_bindings row.
type Binding struct {
	ID        string
	TenantID  string
	Principal Principal
	Kind      BindingKind
	Issuer    string
	Subject   string
	Verified  bool
	Metadata  string // a JSON object, or ""
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// NewBinding is the identifier to bind.
type NewBinding struct {
	Kind    BindingKind
	Issuer  string
	Subject string
	// Verified records that the PRODUCT proved the identifier (a magic link
	// was clicked). The chassis cannot check it and never sets it itself.
	Verified bool
	Metadata string
}

const maxBindingMetadata = 4 << 10

// NormalizeEmail returns the form an address is bound and looked up by:
// trimmed and lowercased. It accepts `local@domain` and nothing fancier —
// no display name, no comment, no quoted local part.
func NormalizeEmail(addr string) (string, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	local, domain, ok := strings.Cut(addr, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") || len(addr) > 254 {
		return "", invalid("email %q: want local@domain", addr)
	}
	for _, r := range addr {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`<>()[]\,;:"`, r) {
			return "", invalid("email %q: unexpected character %q", addr, r)
		}
	}
	return addr, nil
}

func (in NewBinding) normalize() (NewBinding, error) {
	switch {
	case reservedBindingKinds[in.Kind]:
		return in, invalid("binding kind %q is reserved and not built yet", in.Kind)
	case in.Kind == BindEmail:
		if strings.TrimSpace(in.Issuer) != "" {
			return in, invalid("an email binding has no issuer")
		}
		subject, err := NormalizeEmail(in.Subject)
		if err != nil {
			return in, err
		}
		in.Issuer, in.Subject = "", subject
	case in.Kind == BindOIDC:
		in.Issuer, in.Subject = strings.TrimSpace(in.Issuer), strings.TrimSpace(in.Subject)
		if !strings.HasPrefix(in.Issuer, "https://") || len(in.Issuer) > 512 {
			return in, invalid("an oidc binding needs an https:// issuer")
		}
		if in.Subject == "" || len(in.Subject) > 255 {
			return in, invalid("an oidc binding needs a subject (1-255 chars)")
		}
	default:
		return in, invalid("binding kind %q: want %q or %q", in.Kind, BindEmail, BindOIDC)
	}
	if in.Metadata != "" {
		var obj map[string]any
		if len(in.Metadata) > maxBindingMetadata || json.Unmarshal([]byte(in.Metadata), &obj) != nil {
			return in, invalid("binding metadata must be a JSON object of at most %d bytes", maxBindingMetadata)
		}
	}
	return in, nil
}

const bindingCols = `id, tenant_id, principal_id, kind, COALESCE(issuer, ''), subject, verified,
	COALESCE(metadata, ''), created_by, created_at, revoked_at`

func scanBinding(row interface{ Scan(...any) error }) (Binding, error) {
	var (
		b        Binding
		pid      string
		verified int
		created  string
		revoked  sql.NullString
	)
	err := row.Scan(&b.ID, &b.TenantID, &pid, &b.Kind, &b.Issuer, &b.Subject, &verified,
		&b.Metadata, &b.CreatedBy, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, err
	}
	// A stored id was validated on the way in; keep whatever is there.
	kind, _, _ := strings.Cut(pid, ":")
	b.Principal = Principal{ID: pid, Kind: PrincipalKind(kind)}
	b.Verified = verified != 0
	b.CreatedAt = parseStamp(created)
	b.RevokedAt = parseStampPtr(revoked)
	return b, nil
}

// LookupBinding resolves a live identifier to its binding — the first step
// of a login. issuer is "" for email. ErrNotFound when nothing live matches.
func (s *Store) LookupBinding(ctx context.Context, tenantID string, kind BindingKind, issuer, subject string) (Binding, error) {
	if err := requireTenant(tenantID); err != nil {
		return Binding{}, err
	}
	in, err := NewBinding{Kind: kind, Issuer: issuer, Subject: subject}.normalize()
	if err != nil {
		return Binding{}, err
	}
	return s.lookupBinding(ctx, s.DB, tenantID, in)
}

func (s *Store) lookupBinding(ctx context.Context, q querier, tenantID string, in NewBinding) (Binding, error) {
	return scanBinding(s.qr(ctx, q,
		`SELECT `+bindingCols+` FROM principal_bindings
		 WHERE tenant_id = ? AND kind = ? AND COALESCE(issuer, '') = ? AND subject = ? AND revoked_at IS NULL`,
		tenantID, string(in.Kind), in.Issuer, in.Subject))
}

// BindPrincipal binds an identifier to p on behalf of stack. It is the one
// way a binding is written: txco://user/create uses it for the user's email,
// and the protocol account ops will use it for their usernames, so neither
// sees the table.
//
// Binding the same identifier to the same principal again is a no-op that
// returns the existing row (created=false), upgrading verified false→true if
// asked — never the reverse. An identifier live on a different principal is
// ErrBound; a principal owned by another stack is ErrNotOwner.
func (s *Store) BindPrincipal(ctx context.Context, tenantID, stack string, p Principal, nb NewBinding) (Binding, bool, error) {
	if err := requireTenant(tenantID); err != nil {
		return Binding{}, false, err
	}
	in, err := nb.normalize()
	if err != nil {
		return Binding{}, false, err
	}
	base, err := s.authorize(ctx, s.DB, tenantID, stack, p)
	if err != nil {
		return Binding{}, false, err
	}
	// Two passes: a lost insert race surfaces as a unique violation, and the
	// second pass then finds the winner's row.
	for attempt := 0; ; attempt++ {
		existing, err := s.lookupBinding(ctx, s.DB, tenantID, in)
		switch {
		case err == nil:
			if existing.Principal.ID != p.ID {
				return Binding{}, false, ErrBound
			}
			b, verr := s.upgradeVerified(ctx, s.DB, existing, in.Verified)
			return b, false, verr
		case !errors.Is(err, ErrNotFound):
			return Binding{}, false, err
		}
		b, err := s.insertBinding(ctx, s.DB, tenantID, base, p, in)
		if err == nil {
			return b, true, nil
		}
		if attempt > 0 || !s.Dialect.IsUniqueViolationGeneric(err) {
			return Binding{}, false, err
		}
	}
}

func (s *Store) insertBinding(ctx context.Context, q querier, tenantID, base string, p Principal, in NewBinding) (Binding, error) {
	b := Binding{
		ID: "bnd_" + hxid.NewTimeSort().String(), TenantID: tenantID, Principal: p,
		Kind: in.Kind, Issuer: in.Issuer, Subject: in.Subject, Verified: in.Verified,
		Metadata: in.Metadata, CreatedBy: base,
	}
	created := s.stamp()
	b.CreatedAt = parseStamp(created)
	verified := 0
	if in.Verified {
		verified = 1
	}
	_, err := s.ex(ctx, q,
		`INSERT INTO principal_bindings
		   (id, tenant_id, principal_id, kind, issuer, subject, verified, metadata, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, tenantID, p.ID, string(in.Kind), nullIfEmpty(in.Issuer), in.Subject, verified,
		nullIfEmpty(in.Metadata), base, created)
	return b, err
}

// upgradeVerified flips an unverified binding to verified when asked to.
func (s *Store) upgradeVerified(ctx context.Context, q querier, b Binding, verified bool) (Binding, error) {
	if !verified || b.Verified {
		return b, nil
	}
	if _, err := s.ex(ctx, q, `UPDATE principal_bindings SET verified = 1 WHERE id = ?`, b.ID); err != nil {
		return b, err
	}
	b.Verified = true
	return b, nil
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
