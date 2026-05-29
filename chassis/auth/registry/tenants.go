package registry

// Membership storage. The tenants table itself lives in runtime.db and
// is owned by the chassis/tenants package — see `chassis/tenants/store.go`.
// Registry holds the membership-side queries (auth.db); slug enrichment
// goes through a TenantLookup function passed at construction.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Membership mirrors actor_memberships. Capabilities is the decoded
// JSON column — never NULL, always at least one entry. TenantSlug is
// populated by the optional TenantLookup passed to New; when no
// resolver is configured (or it returns ErrNotFound) the slug is left
// empty.
type Membership struct {
	ActorID      string
	TenantID     string
	TenantSlug   string
	Capabilities []string
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

// SetActorSuperAdmin flips the actors.super_admin flag. v1 callers
// only set this during bootstrap (the first enrolled actor) or via a
// future admin command. Idempotent.
func (r *Registry) SetActorSuperAdmin(ctx context.Context, actorID string, super bool) error {
	flag := 0
	if super {
		flag = 1
	}
	_, err := r.ex(ctx, r.DB,
		`UPDATE actors SET super_admin = ? WHERE actor_id = ?`,
		flag, actorID)
	return err
}

// CreateMembership upserts an (actor, tenant) membership. v1 stores
// the capability set as a JSON array; passing the same (actor, tenant)
// pair twice REPLACES the capabilities — useful for "add me to this
// tenant with these capabilities, even if I'm already a member."
//
// Returns the freshly-written membership so callers can echo it
// without a follow-up read. TenantSlug is filled via the registry's
// TenantLookup if one is configured.
func (r *Registry) CreateMembership(ctx context.Context, m Membership) (*Membership, error) {
	if m.ActorID == "" || m.TenantID == "" {
		return nil, errors.New("registry: empty actor_id or tenant_id")
	}
	if len(m.Capabilities) == 0 {
		return nil, errors.New("registry: membership must carry at least one capability")
	}
	capsJSON, err := json.Marshal(m.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("registry: marshal capabilities: %w", err)
	}
	now := m.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Upsert: re-granting an existing membership replaces capabilities
	// and clears any prior revocation. Tenant admins use this to "promote"
	// or "demote" without churning through revoke + re-create.
	_, err = r.ex(ctx, r.DB,
		`INSERT INTO actor_memberships (actor_id, tenant_id, capabilities_json, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(actor_id, tenant_id) DO UPDATE SET
		     capabilities_json = excluded.capabilities_json,
		     created_at        = excluded.created_at,
		     revoked_at        = NULL`,
		m.ActorID, m.TenantID, string(capsJSON), now.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	m.CreatedAt = now.UTC()
	m.RevokedAt = nil
	r.fillSlug(ctx, &m)
	return &m, nil
}

// RevokeMembership soft-deletes a membership by setting revoked_at.
// Idempotent: already-revoked rows are left alone.
func (r *Registry) RevokeMembership(ctx context.Context, actorID, tenantID string) error {
	_, err := r.ex(ctx, r.DB,
		`UPDATE actor_memberships
		    SET revoked_at = ?
		  WHERE actor_id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), actorID, tenantID)
	return err
}

// LoadMembership returns a single (actor, tenant) row. The slug is
// resolved via the registry's TenantLookup; if no resolver is set or
// the lookup fails, TenantSlug is left empty. Returns ErrNotFound when
// no active membership exists.
func (r *Registry) LoadMembership(ctx context.Context, actorID, tenantID string) (*Membership, error) {
	row := r.qr(ctx, r.DB,
		`SELECT actor_id, tenant_id, capabilities_json, created_at, revoked_at
		   FROM actor_memberships
		  WHERE actor_id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		actorID, tenantID)
	m, err := scanMembership(row)
	if err != nil {
		return nil, err
	}
	r.fillSlug(ctx, m)
	return m, nil
}

// ListMembershipsForActor returns every active membership for an
// actor. Slugs are resolved via the registry's TenantLookup; rows are
// sorted by resolved slug (or by tenant_id when slug resolution is
// unavailable) so the caller sees a stable order.
//
// Two-pass shape: drain the membership rows fully *before* calling
// fillSlug. The tenants table lives in runtime.db (a different *sql.DB
// in production, but possibly the same handle under test); resolving
// slugs while the outer Rows is still open would block on a single-
// connection pool, since the cursor is holding the only connection.
func (r *Registry) ListMembershipsForActor(ctx context.Context, actorID string) ([]Membership, error) {
	rows, err := r.qy(ctx, r.DB,
		`SELECT actor_id, tenant_id, capabilities_json, created_at, revoked_at
		   FROM actor_memberships
		  WHERE actor_id = ? AND revoked_at IS NULL`, actorID)
	if err != nil {
		return nil, err
	}
	var out []Membership
	for rows.Next() {
		m, err := scanMembershipRows(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		r.fillSlug(ctx, &out[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TenantSlug == "" && out[j].TenantSlug == "" {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].TenantSlug < out[j].TenantSlug
	})
	return out, nil
}

// ListMembershipsForTenant returns every active member of a tenant.
// Used by `txco auth tenant members <slug>` and for admin auditing.
// Slug is resolved once before the loop (all rows share the same
// tenant) and then stamped onto each row.
func (r *Registry) ListMembershipsForTenant(ctx context.Context, tenantID string) ([]Membership, error) {
	rows, err := r.qy(ctx, r.DB,
		`SELECT actor_id, tenant_id, capabilities_json, created_at, revoked_at
		   FROM actor_memberships
		  WHERE tenant_id = ? AND revoked_at IS NULL
		  ORDER BY created_at ASC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slug := r.slugOf(ctx, tenantID)
	var out []Membership
	for rows.Next() {
		m, err := scanMembershipRows(rows)
		if err != nil {
			return nil, err
		}
		m.TenantSlug = slug
		out = append(out, *m)
	}
	return out, rows.Err()
}

// fillSlug populates m.TenantSlug via the registry's TenantLookup if one
// is configured. Silent on lookup failure — the slug is an enrichment,
// not load-bearing.
func (r *Registry) fillSlug(ctx context.Context, m *Membership) {
	if m == nil {
		return
	}
	m.TenantSlug = r.slugOf(ctx, m.TenantID)
}

func (r *Registry) slugOf(ctx context.Context, tenantID string) string {
	if r == nil || r.Tenants == nil || tenantID == "" {
		return ""
	}
	slug, err := r.Tenants(ctx, tenantID)
	if err != nil {
		return ""
	}
	return slug
}

func scanMembership(row *sql.Row) (*Membership, error) {
	var m Membership
	var capsJSON, created string
	var revoked sql.NullString
	if err := row.Scan(&m.ActorID, &m.TenantID, &capsJSON, &created, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return fillMembership(&m, capsJSON, created, revoked)
}

func scanMembershipRows(rows *sql.Rows) (*Membership, error) {
	var m Membership
	var capsJSON, created string
	var revoked sql.NullString
	if err := rows.Scan(&m.ActorID, &m.TenantID, &capsJSON, &created, &revoked); err != nil {
		return nil, err
	}
	return fillMembership(&m, capsJSON, created, revoked)
}

func fillMembership(m *Membership, capsJSON, created string, revoked sql.NullString) (*Membership, error) {
	if err := json.Unmarshal([]byte(capsJSON), &m.Capabilities); err != nil {
		return nil, fmt.Errorf("registry: parse membership capabilities JSON: %w", err)
	}
	m.CreatedAt = parseTime(created)
	if revoked.Valid {
		rv := parseTime(revoked.String)
		m.RevokedAt = &rv
	}
	return m, nil
}
