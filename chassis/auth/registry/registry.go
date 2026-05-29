// Package registry reads (and minimally mutates) the actor / actor_keys
// / tenants / actor_memberships tables. The auth middleware uses it
// for every signed request; admin endpoints use it for enrolment,
// listing, revocation, and membership management.
package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Registry is the thin façade over the auth tables. It takes a *sql.DB
// directly so it can run inside transactions started by the admin
// handlers (e.g. when enrollment writes an actor + a key + capabilities
// atomically).
//
// Tenants is the cross-DB slug resolver — tenants live in runtime.db
// (chassis/tenants package), and membership queries need slugs for the
// common authz / display paths. May be nil for tests or contexts that
// don't care about slug enrichment; membership methods then leave
// TenantSlug empty.
type Registry struct {
	DB      *sql.DB
	Tenants TenantLookup

	// Dialect adapts the few SQL behaviours that differ between the
	// in-tree SQLite default and a shared Postgres auth store (HA
	// control plane). nil ⇒ SQLite (see dia()).
	Dialect Dialect
}

// TenantLookup resolves a tenant_id to its slug. Satisfied by
// chassis/tenants.Store via the adapter constructed in
// chassis/server/admin/server.go. Function type rather than interface
// because there's exactly one operation and zero state.
type TenantLookup func(ctx context.Context, tenantID string) (string, error)

// New builds a Registry on the SQLite dialect (the in-tree default).
// tenants may be nil — see TenantLookup doc.
func New(db *sql.DB, tenants TenantLookup) *Registry {
	return &Registry{DB: db, Tenants: tenants, Dialect: SQLite}
}

// NewWithDialect builds a Registry on an explicit dialect — used when
// the auth DSN selects Postgres for an HA control plane. A nil dialect
// falls back to SQLite.
func NewWithDialect(db *sql.DB, tenants TenantLookup, d Dialect) *Registry {
	if d == nil {
		d = SQLite
	}
	return &Registry{DB: db, Tenants: tenants, Dialect: d}
}

// dia returns the active dialect, defaulting to SQLite so a
// zero-value/legacy Registry keeps its historical behaviour.
func (r *Registry) dia() Dialect {
	if r.Dialect == nil {
		return SQLite
	}
	return r.Dialect
}

// rb rebinds `?` placeholders for the active dialect (identity on
// SQLite). Every registry statement runs through this.
func (r *Registry) rb(query string) string { return r.dia().Rebind(query) }

// ctxExecer is the common surface of *sql.DB and *sql.Tx the registry
// uses. The ex/qy/qr helpers funnel every statement through r.rb so
// placeholder rebinding happens in exactly one place per call shape.
type ctxExecer interface {
	ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row
}

func (r *Registry) ex(ctx context.Context, e ctxExecer, q string, a ...any) (sql.Result, error) {
	return e.ExecContext(ctx, r.rb(q), a...)
}
func (r *Registry) qy(ctx context.Context, e ctxExecer, q string, a ...any) (*sql.Rows, error) {
	return e.QueryContext(ctx, r.rb(q), a...)
}
func (r *Registry) qr(ctx context.Context, e ctxExecer, q string, a ...any) *sql.Row {
	return e.QueryRowContext(ctx, r.rb(q), a...)
}

// Actor mirrors the actors table row.
//
// Tenant and Stack are vestiges of v1's chassis-wide model and are no
// longer the source of truth for authz scoping — auth_memberships is.
// They linger as nullable columns until phase 4's table rebuild and
// stay readable here for back-compat with existing meta and tooling.
// SuperAdmin is the chassis-wide override: actors with super_admin=1
// pass every RequireCapability regardless of membership.
type Actor struct {
	ActorID    string
	Label      string
	Kind       string
	Subject    string
	Tenant     string
	Stack      string
	SuperAdmin bool
	CreatedAt  time.Time
	RevokedAt  *time.Time
	Meta       string
}

// Key mirrors actor_keys. PublicKey is the raw 32-byte Ed25519 public key.
type Key struct {
	KeyID     string
	ActorID   string
	PublicKey ed25519.PublicKey
	Algorithm string
	CreatedAt time.Time
	RevokedAt *time.Time
	Meta      string
}

// IsRevoked reports whether the key has been revoked (revoked_at IS NOT NULL).
func (k Key) IsRevoked() bool { return k.RevokedAt != nil }

// ErrNotFound is returned by lookups when no matching row exists. The
// caller (middleware) maps this to unknown_key / actor_revoked / etc.
var ErrNotFound = errors.New("not found")

// DefaultTenantID mirrors the seeded tenant_id from
// db/schema/sqlite/runtime/0002_tenants.sql. Kept as a local constant
// (rather than importing chassis/tenants) so the auth registry stays
// independent of the runtime DB's package layout — invitations and
// pre-tenant backfills default to this id, and the value must match
// what the runtime seed inserts.
const DefaultTenantID = "tnt_default"

// ErrKeyAlreadyEnrolled is returned by CreateKey when the public_key
// is already enrolled by a non-revoked actor_keys row (enforced via
// the partial UNIQUE index in migration 0009). Handlers surface this
// as a 409 with the existing actor_id so the caller knows to use
// invite/accept or revoke the old key first.
var ErrKeyAlreadyEnrolled = errors.New("key already enrolled")

// LookupKey reads a single actor_keys row by primary key.
func (r *Registry) LookupKey(ctx context.Context, keyID string) (*Key, error) {
	row := r.qr(ctx, r.DB,
		`SELECT key_id, actor_id, public_key, algorithm, created_at, revoked_at, COALESCE(meta, '')
		 FROM actor_keys WHERE key_id = ?`, keyID)
	return scanKey(row)
}

// LookupActor reads a single actors row by primary key.
func (r *Registry) LookupActor(ctx context.Context, actorID string) (*Actor, error) {
	row := r.qr(ctx, r.DB,
		`SELECT actor_id, COALESCE(label, ''), COALESCE(kind, ''), COALESCE(subject, ''),
		        COALESCE(tenant, ''), COALESCE(stack, ''), super_admin,
		        created_at, revoked_at, COALESCE(meta, '')
		 FROM actors WHERE actor_id = ?`, actorID)
	return scanActor(row)
}

// HasAnyActiveActor reports whether at least one actors row has
// revoked_at IS NULL. The admin server uses this during startup to
// decide whether to auto-generate a first-boot dev-enroll secret; it
// also serves as the burn-after-use gate on `/auth/dev/enroll`.
//
// Cheap by design: LIMIT 1, primary-key scan on a small table.
func (r *Registry) HasAnyActiveActor(ctx context.Context) (bool, error) {
	row := r.qr(ctx, r.DB,
		`SELECT 1 FROM actors WHERE revoked_at IS NULL LIMIT 1`)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListActors returns every row in actors, newest first. v1 returns them
// all; pagination ships in v2 when there's a real need.
func (r *Registry) ListActors(ctx context.Context) ([]Actor, error) {
	rows, err := r.qy(ctx, r.DB,
		`SELECT actor_id, COALESCE(label, ''), COALESCE(kind, ''), COALESCE(subject, ''),
		        COALESCE(tenant, ''), COALESCE(stack, ''), super_admin,
		        created_at, revoked_at, COALESCE(meta, '')
		 FROM actors ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Actor
	for rows.Next() {
		var a Actor
		var revoked sql.NullString
		var created string
		var superAdmin int
		if err := rows.Scan(&a.ActorID, &a.Label, &a.Kind, &a.Subject, &a.Tenant, &a.Stack,
			&superAdmin, &created, &revoked, &a.Meta); err != nil {
			return nil, err
		}
		a.SuperAdmin = superAdmin != 0
		a.CreatedAt = parseTime(created)
		if revoked.Valid {
			t := parseTime(revoked.String)
			a.RevokedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateActor inserts a new actor row. The caller is responsible for
// generating actor_id (use chassis/hxid). Capability rows are written
// separately via GrantCapability so this stays a thin façade.
func (r *Registry) CreateActor(ctx context.Context, a Actor) error {
	if a.ActorID == "" {
		return errors.New("registry: empty actor_id")
	}
	now := a.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := r.ex(ctx, r.DB,
		`INSERT INTO actors (actor_id, label, kind, subject, tenant, stack, created_at, meta)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ActorID, nullable(a.Label), nullable(a.Kind), nullable(a.Subject),
		nullable(a.Tenant), nullable(a.Stack), now.UTC().Format(time.RFC3339), nullable(a.Meta))
	return err
}

// CreateKey inserts an actor_keys row. PublicKey must be a valid
// ed25519.PublicKey (32 bytes).
//
// Returns ErrKeyAlreadyEnrolled if the public_key is already bound to
// a non-revoked actor_keys row (enforced by the partial UNIQUE index
// from migration 0009). Callers should pre-check via
// LookupKeyByPublicKey when they want to branch on the existing row;
// CreateKey detects the race and surfaces the same typed error.
func (r *Registry) CreateKey(ctx context.Context, k Key) error {
	if k.KeyID == "" || k.ActorID == "" {
		return errors.New("registry: empty key_id or actor_id")
	}
	if k.Algorithm == "" {
		k.Algorithm = "ed25519"
	}
	if k.Algorithm == "ed25519" && len(k.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("registry: ed25519 public key must be %d bytes, got %d",
			ed25519.PublicKeySize, len(k.PublicKey))
	}
	now := k.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := r.ex(ctx, r.DB,
		`INSERT INTO actor_keys (key_id, actor_id, public_key, algorithm, created_at, meta)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		k.KeyID, k.ActorID, []byte(k.PublicKey), k.Algorithm,
		now.UTC().Format(time.RFC3339), nullable(k.Meta))
	if err != nil && r.dia().IsUniqueViolation(err) {
		return ErrKeyAlreadyEnrolled
	}
	return err
}

// LookupKeyByPublicKey returns the active actor_keys row whose
// public_key matches the given bytes, or ErrNotFound when no row
// (active or otherwise) carries that key. Used by /auth/dev/enroll
// to refuse re-enrolment and by /auth/invitations/consume to bind
// new memberships to an existing principal instead of minting a
// duplicate actor.
func (r *Registry) LookupKeyByPublicKey(ctx context.Context, pub ed25519.PublicKey) (*Key, error) {
	row := r.qr(ctx, r.DB,
		`SELECT key_id, actor_id, public_key, algorithm, created_at, revoked_at, COALESCE(meta, '')
		   FROM actor_keys
		  WHERE public_key = ? AND revoked_at IS NULL
		  LIMIT 1`, []byte(pub))
	return scanKey(row)
}

// Unique-violation detection moved to the Dialect seam (dialect.go):
// SQLite keeps the historical error-string match; Postgres uses
// SQLSTATE 23505. Call r.dia().IsUniqueViolation(err).

// RevokeKey sets revoked_at on a single key. Idempotent: revoking an
// already-revoked key is a no-op (the column is set to the same now).
func (r *Registry) RevokeKey(ctx context.Context, keyID string) error {
	res, err := r.ex(ctx, r.DB,
		`UPDATE actor_keys SET revoked_at = ? WHERE key_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either already revoked or doesn't exist; check which.
		if _, err := r.LookupKey(ctx, keyID); err != nil {
			return err
		}
	}
	return nil
}

// RevokeActor sets revoked_at on an actor and cascades to all its keys.
// Capability rows are left as-is — the actor's revocation is what
// matters for auth decisions.
func (r *Registry) RevokeActor(ctx context.Context, actorID string) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := r.ex(ctx, tx,
		`UPDATE actors SET revoked_at = ? WHERE actor_id = ? AND revoked_at IS NULL`,
		now, actorID); err != nil {
		return err
	}
	if _, err := r.ex(ctx, tx,
		`UPDATE actor_keys SET revoked_at = ? WHERE actor_id = ? AND revoked_at IS NULL`,
		now, actorID); err != nil {
		return err
	}
	return tx.Commit()
}

// --- invitations ------------------------------------------------------------

// Invitation mirrors the actor_invitations table. Capabilities is the
// decoded JSON array; v1 always materialises ["admin:all"] (no NULL).
// TenantID is the slug the invitation scopes its membership grant to
// — added in migration 0008, backfilled to DefaultTenantID for older
// rows. Empty in only one situation: a pre-0008 invitation that
// somehow escaped the backfill, which the handler treats as default.
type Invitation struct {
	InvitationID string
	TokenHash    string // hex sha-256 of the raw token
	Label        string
	Kind         string
	TenantID     string
	Capabilities []string
	CreatedBy    string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConsumedAt   *time.Time
	ConsumedBy   string
	RevokedAt    *time.Time
}

// HashToken returns the canonical token_hash form used in the schema:
// lowercase hex of SHA-256(token). Exported so handlers and tests share
// the same digest function.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateInvitation inserts an invitation row. The caller is
// responsible for hashing the token and assigning a hxid invitation_id.
// Capabilities must already be set (no defaulting here — that's
// policy, not storage). TenantID defaults to DefaultTenantID when
// empty so callers that haven't yet migrated to tenant-aware invites
// keep working against the seeded default tenant.
func (r *Registry) CreateInvitation(ctx context.Context, inv Invitation) error {
	if inv.InvitationID == "" || inv.TokenHash == "" {
		return errors.New("registry: empty invitation_id or token_hash")
	}
	if len(inv.Capabilities) == 0 {
		return errors.New("registry: invitation must carry at least one capability")
	}
	if inv.CreatedBy == "" {
		return errors.New("registry: invitation missing created_by")
	}
	if inv.TenantID == "" {
		inv.TenantID = DefaultTenantID
	}
	capsJSON, err := json.Marshal(inv.Capabilities)
	if err != nil {
		return fmt.Errorf("registry: marshal capabilities: %w", err)
	}
	now := inv.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err = r.ex(ctx, r.DB,
		`INSERT INTO actor_invitations
			(invitation_id, token_hash, label, kind, tenant_id, capabilities, created_by, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.InvitationID, inv.TokenHash,
		nullable(inv.Label), nullable(inv.Kind),
		inv.TenantID,
		string(capsJSON), inv.CreatedBy,
		now.UTC().Format(time.RFC3339),
		inv.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

// ListInvitations returns every invitation row, newest first. Status
// derivation happens at the HTTP layer — this method is pure storage.
func (r *Registry) ListInvitations(ctx context.Context) ([]Invitation, error) {
	rows, err := r.qy(ctx, r.DB,
		`SELECT invitation_id, token_hash, COALESCE(label, ''), COALESCE(kind, ''),
		        COALESCE(tenant_id, ''),
		        capabilities, created_by, created_at, expires_at,
		        consumed_at, COALESCE(consumed_by, ''), revoked_at
		 FROM actor_invitations
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invitation
	for rows.Next() {
		inv, err := scanInvitationRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	return out, rows.Err()
}

// RevokeInvitation marks an invitation as revoked. Idempotent: revoking
// an already-revoked / already-consumed invitation is a no-op (the
// WHERE clause filters those out).
func (r *Registry) RevokeInvitation(ctx context.Context, invitationID string) error {
	res, err := r.ex(ctx, r.DB,
		`UPDATE actor_invitations
		 SET revoked_at = ?
		 WHERE invitation_id = ? AND revoked_at IS NULL AND consumed_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), invitationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either gone, already revoked, or consumed.
		row := r.qr(ctx, r.DB,
			`SELECT 1 FROM actor_invitations WHERE invitation_id = ?`, invitationID)
		var one int
		if err := row.Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Row exists but is already consumed/revoked: leave it alone, treat as ok.
	}
	return nil
}

// LookupInvitationByTokenHash reads a single invitation row by its
// (already-hashed) token. Used by the unsigned consume endpoint's
// auditing log line; the actual atomic consume goes through
// ConsumeInvitation. Returns ErrNotFound if the row is missing.
func (r *Registry) LookupInvitationByTokenHash(ctx context.Context, tokenHash string) (*Invitation, error) {
	row := r.qr(ctx, r.DB,
		`SELECT invitation_id, token_hash, COALESCE(label, ''), COALESCE(kind, ''),
		        COALESCE(tenant_id, ''),
		        capabilities, created_by, created_at, expires_at,
		        consumed_at, COALESCE(consumed_by, ''), revoked_at
		 FROM actor_invitations WHERE token_hash = ?`, tokenHash)
	return scanInvitationRow(row)
}

// ConsumeResult is the outcome of a successful ConsumeInvitation.
// Reused is true when the consumer's public key was already enrolled
// — in that case ActorID and KeyID point at the EXISTING principal,
// so a single ssh-agent key can collect memberships across many
// tenants without spawning duplicate actor rows.
type ConsumeResult struct {
	Invitation Invitation
	ActorID    string
	KeyID      string
	TenantID   string
	Reused     bool
}

// ConsumeInvitation atomically redeems a token. The whole sequence
// lives in one BEGIN IMMEDIATE transaction so two concurrent
// consumers can't both succeed.
//
// Sequence:
//
//  1. BEGIN IMMEDIATE                          — write lock up front
//  2. Look up actor_keys by public_key (active rows only). If found,
//     the consumer is an EXISTING principal — we'll bind a new
//     membership to them instead of minting a duplicate.
//  3. UPDATE actor_invitations SET consumed_at,consumed_by — guarded
//     by the WHERE clause. consumed_by records the EXISTING actor_id
//     when reusing, otherwise the freshly-minted newActorID.
//  4. Re-read the invitation row to recover capabilities + tenant_id.
//  5. If not reusing: INSERT actors + INSERT actor_keys +
//     INSERT actor_capabilities*N (the chassis-wide back-compat
//     row). If reusing: skip — the principal already has all that.
//  6. Upsert membership(actor_id, invitation.tenant_id,
//     invitation.capabilities) — the tenant grant is the point of
//     this whole flow.
//  7. COMMIT
//
// If anything past step 3 fails, the tx rolls back and the burn is
// reverted — the invitation goes back to "live" and a retry can
// succeed. ErrKeyAlreadyEnrolled bubbles up from a race where another
// consumer enrolled the key between our lookup and our insert; the
// caller's invitation row is rolled back too.
func (r *Registry) ConsumeInvitation(ctx context.Context,
	tokenHash, newActorID, newKeyID string, pub ed25519.PublicKey,
	label, kind string) (*ConsumeResult, error) {

	if tokenHash == "" || newActorID == "" || newKeyID == "" {
		return nil, errors.New("registry: ConsumeInvitation missing required args")
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("registry: ed25519 public key must be %d bytes, got %d",
			ed25519.PublicKeySize, len(pub))
	}

	// Upfront write lock via the dialect: SQLite issues BEGIN
	// IMMEDIATE; Postgres opens a SERIALIZABLE tx. The single-winner
	// guarantee itself comes from the conditional UPDATE below, not
	// the lock — the lock just fails fast instead of deadlocking.
	tx, err := r.dia().BeginImmediate(ctx, r.DB)
	if err != nil {
		return nil, fmt.Errorf("registry: begin immediate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowStr := time.Now().UTC().Format(time.RFC3339)

	// Pre-burn-lookup: if we recognise the pubkey, we'll record the
	// consume against the EXISTING actor and skip the actor/key
	// inserts below. The lookup runs inside the same tx so we see a
	// consistent snapshot relative to any concurrent enroll.
	existing, lookupErr := r.lookupKeyTx(ctx, tx, pub)
	if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
		return nil, fmt.Errorf("registry: lookup key: %w", lookupErr)
	}

	consumedBy := newActorID
	if existing != nil {
		consumedBy = existing.ActorID
	}

	res, err := r.ex(ctx, tx,
		`UPDATE actor_invitations
		   SET consumed_at = ?, consumed_by = ?
		 WHERE token_hash = ?
		   AND consumed_at IS NULL
		   AND revoked_at  IS NULL
		   AND expires_at  > ?`,
		nowStr, consumedBy, tokenHash, nowStr)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, ErrNotFound
	}

	row := r.qr(ctx, tx,
		`SELECT invitation_id, token_hash, COALESCE(label, ''), COALESCE(kind, ''),
		        COALESCE(tenant_id, ''),
		        capabilities, created_by, created_at, expires_at,
		        consumed_at, COALESCE(consumed_by, ''), revoked_at
		 FROM actor_invitations WHERE token_hash = ?`, tokenHash)
	inv, err := scanInvitationRow(row)
	if err != nil {
		return nil, fmt.Errorf("registry: re-read invitation: %w", err)
	}
	tenantID := inv.TenantID
	if tenantID == "" {
		// Defensive: a pre-0008 invitation that escaped the backfill.
		// Land it in the default tenant rather than rejecting outright.
		tenantID = DefaultTenantID
	}

	out := &ConsumeResult{
		Invitation: *inv,
		TenantID:   tenantID,
	}

	if existing != nil {
		out.ActorID = existing.ActorID
		out.KeyID = existing.KeyID
		out.Reused = true
	} else {
		out.ActorID = newActorID
		out.KeyID = newKeyID

		if _, err := r.ex(ctx, tx,
			`INSERT INTO actors (actor_id, label, kind, created_at) VALUES (?, ?, ?, ?)`,
			newActorID, nullable(label), nullable(kind), nowStr); err != nil {
			return nil, fmt.Errorf("registry: insert actor: %w", err)
		}
		if _, err := r.ex(ctx, tx,
			`INSERT INTO actor_keys (key_id, actor_id, public_key, algorithm, created_at)
			 VALUES (?, ?, ?, 'ed25519', ?)`,
			newKeyID, newActorID, []byte(pub), nowStr); err != nil {
			if r.dia().IsUniqueViolation(err) {
				// Race: another consumer enrolled this key between
				// our lookup and our insert. The UPDATE above already
				// burned this invitation, so we surface the typed
				// error and let the caller's retry start clean (the
				// rollback unburns it).
				return nil, ErrKeyAlreadyEnrolled
			}
			return nil, fmt.Errorf("registry: insert key: %w", err)
		}
		// Phase 8b retired actor_capabilities. New actors get their
		// permissions from actor_memberships only; the membership
		// insert below is what binds the invitation's caps in the
		// invitation's tenant.
	}

	// Upsert the membership in the invitation's tenant. Same row
	// whether we reused or minted — this is what the tenant
	// middleware reads at sign time to scope capabilities.
	capsJSON, err := json.Marshal(inv.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("registry: marshal membership caps: %w", err)
	}
	if _, err := r.ex(ctx, tx,
		`INSERT INTO actor_memberships (actor_id, tenant_id, capabilities_json, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(actor_id, tenant_id) DO UPDATE SET
		     capabilities_json = excluded.capabilities_json,
		     created_at        = excluded.created_at,
		     revoked_at        = NULL`,
		out.ActorID, tenantID, string(capsJSON), nowStr); err != nil {
		return nil, fmt.Errorf("registry: insert membership: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("registry: commit consume: %w", err)
	}
	return out, nil
}

// lookupKeyTx is the tx-local variant of LookupKeyByPublicKey used by
// ConsumeInvitation so the read and the subsequent writes share one
// snapshot. Returns ErrNotFound when no active row exists.
func (r *Registry) lookupKeyTx(ctx context.Context, tx *sql.Tx, pub ed25519.PublicKey) (*Key, error) {
	row := r.qr(ctx, tx,
		`SELECT key_id, actor_id, public_key, algorithm, created_at, revoked_at, COALESCE(meta, '')
		   FROM actor_keys
		  WHERE public_key = ? AND revoked_at IS NULL
		  LIMIT 1`, []byte(pub))
	return scanKey(row)
}

// scanInvitationRow / scanInvitationRows decode an actor_invitations
// row. Separate variants for *sql.Row and *sql.Rows to keep type
// signatures honest (Row.Scan returns sql.ErrNoRows; Rows.Scan does
// not).
func scanInvitationRow(row *sql.Row) (*Invitation, error) {
	var (
		inv        Invitation
		capsJSON   string
		createdStr string
		expiresStr string
		consumedNS sql.NullString
		revokedNS  sql.NullString
	)
	err := row.Scan(&inv.InvitationID, &inv.TokenHash, &inv.Label, &inv.Kind,
		&inv.TenantID,
		&capsJSON, &inv.CreatedBy, &createdStr, &expiresStr,
		&consumedNS, &inv.ConsumedBy, &revokedNS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return fillInvitation(&inv, capsJSON, createdStr, expiresStr, consumedNS, revokedNS)
}

func scanInvitationRows(rows *sql.Rows) (*Invitation, error) {
	var (
		inv        Invitation
		capsJSON   string
		createdStr string
		expiresStr string
		consumedNS sql.NullString
		revokedNS  sql.NullString
	)
	if err := rows.Scan(&inv.InvitationID, &inv.TokenHash, &inv.Label, &inv.Kind,
		&inv.TenantID,
		&capsJSON, &inv.CreatedBy, &createdStr, &expiresStr,
		&consumedNS, &inv.ConsumedBy, &revokedNS); err != nil {
		return nil, err
	}
	return fillInvitation(&inv, capsJSON, createdStr, expiresStr, consumedNS, revokedNS)
}

func fillInvitation(inv *Invitation, capsJSON, createdStr, expiresStr string,
	consumedNS, revokedNS sql.NullString) (*Invitation, error) {
	if err := json.Unmarshal([]byte(capsJSON), &inv.Capabilities); err != nil {
		return nil, fmt.Errorf("registry: parse capabilities JSON: %w", err)
	}
	inv.CreatedAt = parseTime(createdStr)
	inv.ExpiresAt = parseTime(expiresStr)
	if consumedNS.Valid {
		t := parseTime(consumedNS.String)
		inv.ConsumedAt = &t
	}
	if revokedNS.Valid {
		t := parseTime(revokedNS.String)
		inv.RevokedAt = &t
	}
	return inv, nil
}

// scanKey is shared between LookupKey and the (future) bulk listing.
func scanKey(row *sql.Row) (*Key, error) {
	var k Key
	var pub []byte
	var revoked sql.NullString
	var created string
	if err := row.Scan(&k.KeyID, &k.ActorID, &pub, &k.Algorithm, &created, &revoked, &k.Meta); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.PublicKey = ed25519.PublicKey(pub)
	k.CreatedAt = parseTime(created)
	if revoked.Valid {
		t := parseTime(revoked.String)
		k.RevokedAt = &t
	}
	return &k, nil
}

func scanActor(row *sql.Row) (*Actor, error) {
	var a Actor
	var revoked sql.NullString
	var created string
	var superAdmin int
	if err := row.Scan(&a.ActorID, &a.Label, &a.Kind, &a.Subject, &a.Tenant, &a.Stack,
		&superAdmin, &created, &revoked, &a.Meta); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.SuperAdmin = superAdmin != 0
	a.CreatedAt = parseTime(created)
	if revoked.Valid {
		t := parseTime(revoked.String)
		a.RevokedAt = &t
	}
	return &a, nil
}

// parseTime accepts the historical RFC3339 format plus the
// milli-precision shape used by the versioned-opstacks migrations
// (`2006-01-02T15:04:05.000Z`). Falls back to the zero value so the
// auth middleware doesn't crash on a row with a malformed timestamp;
// downstream IsValid/IsRevoked checks treat the zero time the same
// as "no timestamp", which is the safe default.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// nullable returns nil for empty strings so we don't write "" into a
// nullable column when the caller didn't supply one.
func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
