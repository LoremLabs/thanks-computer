// Package tenants owns the tenants and tenant_hostnames tables in
// runtime.db.
//
// Tenants are routing topology — slug, name, and the per-hostname
// mapping the data-plane router uses to resolve `Host: foo.local`
// → (tenant_id, stack). They live in runtime.db so a data-plane-only
// chassis can resolve them without opening auth.db.
//
// Identity-side tables in auth.db (memberships, invitations, browser
// sessions, etc.) reference tenant_id as opaque TEXT; the cross-DB
// integrity is by-convention since SQLite cannot enforce FKs across
// files without ATTACH DATABASE.
package tenants

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

// defaultHxidNew is the production-time-sortable hxid generator. Kept
// as a function var (hxidNew) so tests can stub it.
func defaultHxidNew() string { return hxid.New().String() }

// Tenant mirrors the tenants table row.
type Tenant struct {
	TenantID  string
	Slug      string
	Name      string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Hostname mirrors a tenant_hostnames row.
type Hostname struct {
	ID         string
	Hostname   string
	TenantID   string
	Stack      string
	CreatedAt  time.Time
	CreatedBy  string
	RevokedAt  *time.Time
	VerifiedAt *time.Time
	// Per-host DKIM material (0017), set only on chassis-minted structured
	// hosts (created_by = SystemStructuredHostCreatedBy). Empty for custom
	// domains (which use their dns_zones key). Carried by the loaders + the
	// fleet row builder so per-host signing/publishing stays consistent.
	DKIMSelector   string
	DKIMPrivatePEM string
	DKIMPublicB64  string
}

// Challenge mirrors a tenant_hostname_challenges row — the proof-of-
// ownership artifact created by `POST /hostnames/{host}/challenges`.
type Challenge struct {
	ID          string
	HostnameID  string
	Method      string
	Token       string
	CreatedAt   time.Time
	CreatedBy   string
	ExpiresAt   time.Time
	AttemptedAt *time.Time
	LastError   string
	VerifiedAt  *time.Time
	RevokedAt   *time.Time
}

// ErrChallengeExpired signals the active challenge for (hostname,
// method) has aged past its expires_at. Caller should re-issue.
var ErrChallengeExpired = errors.New("challenge expired")

// Store is the thin façade over the tenants table. Backed by runtime.db.
type Store struct {
	DB *sql.DB
	// dialect makes the runtime writers portable to Postgres. nil ⇒ SQLite
	// (see dia()). Always the runtime DB's dialect, since s.DB is the
	// authoritative runtime store — never the SQLite dbcache mirror.
	dialect registry.Dialect
}

// New builds a Store against the given runtime *sql.DB, defaulting to the
// SQLite dialect (the open-core / single-node build). Cloud callers that
// run the runtime store on Postgres use NewWithDialect.
func New(db *sql.DB) *Store { return NewWithDialect(db, nil) }

// NewWithDialect builds a Store with an explicit SQL dialect. A nil dialect
// defaults to SQLite, so existing callers stay byte-identical.
func NewWithDialect(db *sql.DB, d registry.Dialect) *Store {
	return &Store{DB: db, dialect: orSQLite(d)}
}

// orSQLite normalizes a nil dialect to SQLite. Package-level read/write
// funcs that source from the dbcache mirror (always a SQLite :memory: dump)
// pass nil; authoritative-RuntimeDB callers pass the runtime dialect.
func orSQLite(d registry.Dialect) registry.Dialect {
	if d == nil {
		return registry.SQLite
	}
	return d
}

func (s *Store) dia() registry.Dialect { return orSQLite(s.dialect) }

// rb rebinds `?` placeholders for the store's dialect (identity on SQLite).
func (s *Store) rb(q string) string { return s.dia().Rebind(q) }

// exec/query/queryRow run a statement on s.DB with placeholders rebound to
// the store's dialect. Tx-scoped methods (…Tx) rebind inline via s.rb since
// they run over a caller-supplied *sql.Tx rather than s.DB.
func (s *Store) exec(ctx context.Context, q string, a ...any) (sql.Result, error) {
	return s.DB.ExecContext(ctx, s.rb(q), a...)
}
func (s *Store) query(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	return s.DB.QueryContext(ctx, s.rb(q), a...)
}
func (s *Store) queryRow(ctx context.Context, q string, a ...any) *sql.Row {
	return s.DB.QueryRowContext(ctx, s.rb(q), a...)
}

// ErrNotFound mirrors the same sentinel the auth registry uses; callers
// in admin handlers translate it to HTTP 404.
var ErrNotFound = errors.New("not found")

// ErrHostnameInUse signals that an active row already exists for the
// requested hostname (partial unique index collision). The caller
// translates this to HTTP 409 and exposes the prior owner's tenant_id
// via the conflict body.
var ErrHostnameInUse = errors.New("hostname in use")

// DefaultTenantSlug is seeded by db/schema/sqlite/runtime/0002_tenants.sql.
// The first bootstrap actor claims it via the super_admin path.
const DefaultTenantSlug = "default"

// DefaultTenantID is the seeded primary key. Stable so callers can
// reference it without a slug round-trip.
const DefaultTenantID = "tnt_default"

// SystemTenantSlug owns the chassis ingress-fallback namespace
// (`boot/*`). Requests that match no ingress route are stamped with
// this tenant and run pinned to it, so the data-plane op lookup is
// tenant-filtered like every other request (no global/unfiltered
// path). A `_sys` boot rule may re-tenant a request into a real
// tenant — the only place the request's pinned tenant may change, and
// only one-way (see processor's re-tenant gate). Seeded by
// db/schema/sqlite/runtime/0007_system_tenant.sql.
const SystemTenantSlug = "_sys"

// SystemTenantID is the system tenant's seeded primary key.
const SystemTenantID = "tnt_sys"

// ReservedSlug reports whether a slug is reserved and may not be
// claimed by a tenant. The whole `_`-prefixed namespace is reserved
// for chassis-internal tenants (e.g. `_sys`) so operator-owned system
// tenants can never collide with or be impersonated by a created
// tenant. Caller should pass the already-normalised (lower/trim) slug.
func ReservedSlug(slug string) bool {
	return strings.HasPrefix(slug, "_")
}

// Create inserts a new tenants row. Caller supplies the pre-generated
// tenant_id (hxid "tnt_…") and a slug. Slug is lower-cased and trimmed
// before insert because the UNIQUE index is case-sensitive.
func (s *Store) Create(ctx context.Context, t Tenant) error {
	if t.TenantID == "" {
		return errors.New("tenants: empty tenant_id")
	}
	slug := strings.ToLower(strings.TrimSpace(t.Slug))
	if slug == "" {
		return errors.New("tenants: empty tenant slug")
	}
	// Defence in depth: the create endpoint rejects reserved slugs with
	// a clean 400, but the Store is also reachable from the CLI server
	// path, so guard here too. The migration that seeds `_sys` inserts
	// directly and bypasses this path.
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
	_, err := s.exec(ctx,
		`INSERT INTO tenants (tenant_id, slug, name, created_at)
		 VALUES (?, ?, ?, ?)`,
		t.TenantID, slug, nameArg, now.UTC().Format(time.RFC3339))
	return err
}

// Lookup reads a tenants row by primary key. Returns ErrNotFound when
// no row exists; satisfies the registry.TenantLookup interface so the
// auth registry can resolve slugs without importing this package.
func (s *Store) Lookup(ctx context.Context, tenantID string) (*Tenant, error) {
	row := s.queryRow(ctx,
		`SELECT tenant_id, slug, COALESCE(name, ''), created_at, revoked_at
		   FROM tenants WHERE tenant_id = ?`, tenantID)
	return scan(row)
}

// LookupBySlug resolves a slug to a tenant. Used by the admin mux when
// it sees `/v1/tenants/{slug}/…`.
func (s *Store) LookupBySlug(ctx context.Context, slug string) (*Tenant, error) {
	row := s.queryRow(ctx,
		`SELECT tenant_id, slug, COALESCE(name, ''), created_at, revoked_at
		   FROM tenants WHERE slug = ?`,
		strings.ToLower(strings.TrimSpace(slug)))
	return scan(row)
}

// List returns every non-revoked tenant, newest first.
func (s *Store) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.query(ctx,
		`SELECT tenant_id, slug, COALESCE(name, ''), created_at, revoked_at
		   FROM tenants
		  WHERE revoked_at IS NULL
		  ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		var revoked sql.NullString
		var created string
		if err := rows.Scan(&t.TenantID, &t.Slug, &t.Name, &created, &revoked); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTime(created)
		if revoked.Valid {
			rv := parseTime(revoked.String)
			t.RevokedAt = &rv
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Hostnames -------------------------------------------------------

// CreateHostname canonicalizes the supplied hostname, generates an
// hxid "thn_…", and inserts. On collision with an active row (driven
// by the partial unique index on hostname WHERE revoked_at IS NULL)
// returns ErrHostnameInUse and stamps the conflicting row's TenantID
// onto the returned Hostname so the caller can render a 409 with the
// prior owner's id.
//
// Caller-supplied fields used: Hostname, TenantID, Stack, CreatedBy.
// Other fields (ID, CreatedAt) are filled in here.
func (s *Store) CreateHostname(ctx context.Context, h Hostname) (Hostname, error) {
	canon, ok := CanonicalizeHost(h.Hostname)
	if !ok || !IsValidHostname(canon) {
		return Hostname{}, errors.New("tenants: invalid hostname")
	}
	if h.TenantID == "" {
		return Hostname{}, errors.New("tenants: empty tenant_id")
	}
	// Stack is now OPTIONAL — a hostname row can be claimed without a
	// routing target (the Vercel model). The caller decides whether
	// to provide one upfront (shortcut) or attach later via the
	// /attach endpoint. Routing requires both verified_at and a
	// non-empty stack; the DBResolver enforces both at request time.
	now := time.Now().UTC()
	id := "thn_" + hxid.New().String()

	var createdByArg any
	if h.CreatedBy != "" {
		createdByArg = h.CreatedBy
	}

	_, err := s.exec(ctx,
		`INSERT INTO tenant_hostnames
		     (id, hostname, tenant_id, stack, created_at, created_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, canon, h.TenantID, h.Stack, now.Format(time.RFC3339), createdByArg)
	if err != nil {
		if s.dia().IsUniqueViolationGeneric(err) {
			// Surface the existing owner so the caller can render the
			// 409 body — the index only constrains active rows, so
			// the active row is unambiguous.
			if existing, lookupErr := s.LookupActiveHostname(ctx, canon); lookupErr == nil {
				return existing, ErrHostnameInUse
			}
			return Hostname{}, ErrHostnameInUse
		}
		return Hostname{}, err
	}

	return Hostname{
		ID:        id,
		Hostname:  canon,
		TenantID:  h.TenantID,
		Stack:     h.Stack,
		CreatedAt: now,
		CreatedBy: h.CreatedBy,
	}, nil
}

// ListHostnames returns hostnames for a tenant. When includeRevoked is
// false (the default for admin list endpoints) only currently-active
// rows are returned, ordered by hostname for stable shell-script
// consumption. When true, revoked rows are included for "who used to
// own this?" debugging, ordered by created_at DESC so the most recent
// rows appear first.
func (s *Store) ListHostnames(ctx context.Context, tenantID string, includeRevoked bool) ([]Hostname, error) {
	var query string
	if includeRevoked {
		query = `SELECT id, hostname, tenant_id, stack, created_at,
		                COALESCE(created_by, ''), revoked_at, verified_at, dkim_selector, dkim_private_pem, dkim_public_b64
		           FROM tenant_hostnames
		          WHERE tenant_id = ?
		          ORDER BY created_at DESC`
	} else {
		query = `SELECT id, hostname, tenant_id, stack, created_at,
		                COALESCE(created_by, ''), revoked_at, verified_at, dkim_selector, dkim_private_pem, dkim_public_b64
		           FROM tenant_hostnames
		          WHERE tenant_id = ? AND revoked_at IS NULL
		          ORDER BY hostname`
	}
	rows, err := s.query(ctx, query, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hostname
	for rows.Next() {
		h, err := scanHostname(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AttachHostname sets the stack on an existing active row, identified
// by canonical hostname. Stack must be non-empty (call RevokeHostname
// to remove the row, or pass an empty stack to detach in a future
// version). Returns ErrNotFound if no active row matches.
//
// The caller is responsible for verifying that the stack exists in
// the same tenant — the store doesn't reach into the stacks table.
func (s *Store) AttachHostname(ctx context.Context, hostname, stack string) error {
	canon, ok := CanonicalizeHost(hostname)
	if !ok {
		return ErrNotFound
	}
	if stack == "" {
		return errors.New("tenants: empty stack")
	}
	res, err := s.exec(ctx,
		`UPDATE tenant_hostnames
		    SET stack = ?
		  WHERE hostname = ? AND revoked_at IS NULL`,
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

// RevokeHostname soft-deletes the active row matching the canonical
// hostname. Idempotent — revoking an absent or already-revoked
// hostname returns nil with no row affected.
func (s *Store) RevokeHostname(ctx context.Context, hostname string) error {
	canon, ok := CanonicalizeHost(hostname)
	if !ok {
		return nil // lenient: nothing to revoke if it can't even canonicalize
	}
	_, err := s.exec(ctx,
		`UPDATE tenant_hostnames
		    SET revoked_at = ?
		  WHERE hostname = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), canon)
	return err
}

// LookupActiveHostname is the read-side lookup: hostname →
// (Hostname row, Tenant row). The resolver in chassis/server/ingress
// does its own JOIN against the dbcache mirror for the data-plane hot
// path; this method is for admin handlers that want the same shape
// without re-implementing the JOIN.
func (s *Store) LookupActiveHostname(ctx context.Context, hostname string) (Hostname, error) {
	canon, ok := CanonicalizeHost(hostname)
	if !ok {
		return Hostname{}, ErrNotFound
	}
	row := s.queryRow(ctx,
		`SELECT id, hostname, tenant_id, stack, created_at,
		        COALESCE(created_by, ''), revoked_at, verified_at, dkim_selector, dkim_private_pem, dkim_public_b64
		   FROM tenant_hostnames
		  WHERE hostname = ? AND revoked_at IS NULL`, canon)
	var h Hostname
	var revoked, verified sql.NullString
	var created string
	if err := row.Scan(&h.ID, &h.Hostname, &h.TenantID, &h.Stack, &created, &h.CreatedBy, &revoked, &verified, &h.DKIMSelector, &h.DKIMPrivatePEM, &h.DKIMPublicB64); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Hostname{}, ErrNotFound
		}
		return Hostname{}, err
	}
	h.CreatedAt = parseTime(created)
	if revoked.Valid {
		rv := parseTime(revoked.String)
		h.RevokedAt = &rv
	}
	if verified.Valid {
		vt := parseTime(verified.String)
		h.VerifiedAt = &vt
	}
	return h, nil
}

func scanHostname(rows *sql.Rows) (Hostname, error) {
	var h Hostname
	var revoked, verified sql.NullString
	var created string
	if err := rows.Scan(&h.ID, &h.Hostname, &h.TenantID, &h.Stack, &created, &h.CreatedBy, &revoked, &verified, &h.DKIMSelector, &h.DKIMPrivatePEM, &h.DKIMPublicB64); err != nil {
		return Hostname{}, err
	}
	h.CreatedAt = parseTime(created)
	if revoked.Valid {
		rv := parseTime(revoked.String)
		h.RevokedAt = &rv
	}
	if verified.Valid {
		vt := parseTime(verified.String)
		h.VerifiedAt = &vt
	}
	return h, nil
}

// --- Challenges ------------------------------------------------------

// ChallengeTTL is how long a freshly-issued challenge stays valid.
// After expiry, /verify returns "expired, re-issue first." 24h matches
// the operator expectation set by ACME and is long enough for one DNS
// propagation cycle.
const ChallengeTTL = 24 * time.Hour

// CreateChallenge issues a fresh challenge for (hostnameID, method).
// Any existing active challenge for that pair is soft-revoked first
// — both moves happen in one BEGIN IMMEDIATE transaction so the
// partial unique index never sees two active rows.
//
// `tokenGen` is the source of the secret token (160 bits). Passed in
// so tests can pin it. Production callers use a crypto/rand-backed
// generator. The token MUST be unique across all challenges (the
// UNIQUE constraint on tenant_hostname_challenges.token enforces it,
// but generating 160 bits makes the collision risk negligible).
func (s *Store) CreateChallenge(ctx context.Context, hostnameID, method, createdBy, token string) (Challenge, error) {
	if hostnameID == "" {
		return Challenge{}, errors.New("tenants: empty hostname_id")
	}
	if method != "dns-txt" && method != "http-01" {
		return Challenge{}, fmt.Errorf("tenants: unknown challenge method %q", method)
	}
	if token == "" {
		return Challenge{}, errors.New("tenants: empty token")
	}

	now := time.Now().UTC()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Challenge{}, err
	}
	defer tx.Rollback()

	// Soft-revoke any prior active challenge for this pair.
	if _, err := tx.ExecContext(ctx,
		s.rb(`UPDATE tenant_hostname_challenges
		    SET revoked_at = ?
		  WHERE hostname_id = ? AND method = ?
		    AND verified_at IS NULL AND revoked_at IS NULL`),
		now.Format(time.RFC3339), hostnameID, method); err != nil {
		return Challenge{}, fmt.Errorf("revoke prior: %w", err)
	}

	id := "thc_" + hxidNew()
	expiresAt := now.Add(ChallengeTTL)
	var createdByArg any
	if createdBy != "" {
		createdByArg = createdBy
	}
	if _, err := tx.ExecContext(ctx,
		s.rb(`INSERT INTO tenant_hostname_challenges
		     (id, hostname_id, method, token, created_at, created_by, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		id, hostnameID, method, token,
		now.Format(time.RFC3339), createdByArg, expiresAt.Format(time.RFC3339),
	); err != nil {
		return Challenge{}, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Challenge{}, err
	}
	return Challenge{
		ID:         id,
		HostnameID: hostnameID,
		Method:     method,
		Token:      token,
		CreatedAt:  now,
		CreatedBy:  createdBy,
		ExpiresAt:  expiresAt,
	}, nil
}

// ActiveChallenge returns the live (non-verified, non-revoked)
// challenge for (hostnameID, method). Returns ErrNotFound when none
// exists, ErrChallengeExpired when the row exists but its expires_at
// has passed.
func (s *Store) ActiveChallenge(ctx context.Context, hostnameID, method string) (*Challenge, error) {
	row := s.queryRow(ctx,
		`SELECT id, hostname_id, method, token, created_at,
		        COALESCE(created_by, ''), expires_at, attempted_at,
		        COALESCE(last_error, ''), verified_at, revoked_at
		   FROM tenant_hostname_challenges
		  WHERE hostname_id = ? AND method = ?
		    AND verified_at IS NULL AND revoked_at IS NULL`,
		hostnameID, method)
	c, err := scanChallenge(row)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(c.ExpiresAt) {
		return c, ErrChallengeExpired
	}
	return c, nil
}

// LookupChallengeByToken is the read-side lookup for the
// /.well-known/txco-verify/{token} handler. Returns the active
// challenge for the token, ErrNotFound if no row matches, or
// ErrChallengeExpired if the matching row exists but is past its
// expires_at.
func (s *Store) LookupChallengeByToken(ctx context.Context, token string) (*Challenge, error) {
	row := s.queryRow(ctx,
		`SELECT id, hostname_id, method, token, created_at,
		        COALESCE(created_by, ''), expires_at, attempted_at,
		        COALESCE(last_error, ''), verified_at, revoked_at
		   FROM tenant_hostname_challenges
		  WHERE token = ? AND verified_at IS NULL AND revoked_at IS NULL`,
		token)
	c, err := scanChallenge(row)
	if err != nil {
		return nil, err
	}
	if time.Now().UTC().After(c.ExpiresAt) {
		return c, ErrChallengeExpired
	}
	return c, nil
}

// RecordChallengeAttempt updates attempted_at + last_error after a
// verification attempt. When verified is true, the row's verified_at
// is also set to now (and last_error cleared); callers should
// follow with MarkHostnameVerified to flip the parent row.
func (s *Store) RecordChallengeAttempt(ctx context.Context, id, lastError string, verified bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if verified {
		_, err := s.exec(ctx,
			`UPDATE tenant_hostname_challenges
			    SET attempted_at = ?, last_error = NULL, verified_at = ?
			  WHERE id = ?`,
			now, now, id)
		return err
	}
	// Truncate the error so a misbehaving DNS resolver can't pour
	// 4KB into the row. Re-validate after the byte cut: splitting a
	// multi-byte rune (the error text embeds attacker-influenceable DNS
	// TXT / http-01 bytes) leaves invalid UTF-8, which Postgres TEXT
	// rejects — failing the bookkeeping UPDATE itself.
	if len(lastError) > 512 {
		lastError = lastError[:512]
	}
	lastError = strings.ToValidUTF8(lastError, "")
	_, err := s.exec(ctx,
		`UPDATE tenant_hostname_challenges
		    SET attempted_at = ?, last_error = ?
		  WHERE id = ?`,
		now, lastError, id)
	return err
}

// MarkHostnameVerified flips the parent row's verified_at to `when`.
// Called after RecordChallengeAttempt with verified=true so the
// resolver can see the hostname as verified on its next dbcache
// reload.
func (s *Store) MarkHostnameVerified(ctx context.Context, hostnameID string, when time.Time) error {
	_, err := s.exec(ctx,
		`UPDATE tenant_hostnames SET verified_at = ? WHERE id = ?`,
		when.UTC().Format(time.RFC3339), hostnameID)
	return err
}

func scanChallenge(row *sql.Row) (*Challenge, error) {
	var c Challenge
	var attempted, verified, revoked sql.NullString
	var created, expires string
	err := row.Scan(&c.ID, &c.HostnameID, &c.Method, &c.Token,
		&created, &c.CreatedBy, &expires,
		&attempted, &c.LastError, &verified, &revoked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = parseTime(created)
	c.ExpiresAt = parseTime(expires)
	if attempted.Valid {
		at := parseTime(attempted.String)
		c.AttemptedAt = &at
	}
	if verified.Valid {
		vt := parseTime(verified.String)
		c.VerifiedAt = &vt
	}
	if revoked.Valid {
		rv := parseTime(revoked.String)
		c.RevokedAt = &rv
	}
	return &c, nil
}

// hxidNew is a small wrapper around the hxid package so tests can
// stub. Production uses the time-sortable hxid.
var hxidNew = defaultHxidNew

func scan(row *sql.Row) (*Tenant, error) {
	var t Tenant
	var revoked sql.NullString
	var created string
	if err := row.Scan(&t.TenantID, &t.Slug, &t.Name, &created, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.CreatedAt = parseTime(created)
	if revoked.Valid {
		rv := parseTime(revoked.String)
		t.RevokedAt = &rv
	}
	return &t, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
