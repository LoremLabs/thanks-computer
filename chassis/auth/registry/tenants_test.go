package registry

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Membership-side tests. Tenant CRUD lives in chassis/tenants/store_test.go
// now that the tenants table is owned by that package. Here we seed
// tenants via raw SQL and wire a slug-resolver into the Registry so the
// post-split TenantLookup path is exercised end-to-end without
// importing chassis/tenants from auth/registry tests.

// seedTenant inserts a row directly into the in-memory tenants table.
// The Registry tests don't need to round-trip through tenants.Store —
// they just need a row to resolve against via the configured
// TenantLookup.
func seedTenant(t *testing.T, db *sql.DB, tenantID, slug string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at)
		 VALUES (?, ?, '2026-01-01T00:00:00Z')`,
		tenantID, slug); err != nil {
		t.Fatalf("seed tenant %s: %v", tenantID, err)
	}
}

// dbSlugLookup is the test-local equivalent of the production wiring:
// resolve tenant_id → slug from the same DB the registry is reading.
// Returns ErrNotFound (matching production semantics) when the tenant
// is missing.
func dbSlugLookup(db *sql.DB) TenantLookup {
	return func(ctx context.Context, tenantID string) (string, error) {
		var slug string
		err := db.QueryRowContext(ctx,
			`SELECT slug FROM tenants WHERE tenant_id = ?`, tenantID).Scan(&slug)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return slug, err
	}
}

// newRegistryWithSlugs builds a Registry whose TenantLookup is wired
// to the same in-memory DB the registry reads from. Tests that need
// the membership.TenantSlug field populated use this; tests that don't
// can still call New(db, nil) directly.
func newRegistryWithSlugs(t *testing.T) (*Registry, *sql.DB) {
	t.Helper()
	db := newRegistryDB(t)
	return New(db, dbSlugLookup(db)), db
}

// TestSetActorSuperAdmin — flip the flag, read it back via LookupActor.
func TestSetActorSuperAdmin(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_a", Label: "admin"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	a, err := r.LookupActor(ctx, "actor_a")
	if err != nil {
		t.Fatalf("LookupActor: %v", err)
	}
	if a.SuperAdmin {
		t.Fatalf("super_admin default should be false")
	}
	if err := r.SetActorSuperAdmin(ctx, "actor_a", true); err != nil {
		t.Fatalf("SetActorSuperAdmin: %v", err)
	}
	a, _ = r.LookupActor(ctx, "actor_a")
	if !a.SuperAdmin {
		t.Errorf("super_admin not set")
	}
	// Idempotent flip back.
	_ = r.SetActorSuperAdmin(ctx, "actor_a", false)
	a, _ = r.LookupActor(ctx, "actor_a")
	if a.SuperAdmin {
		t.Errorf("super_admin not cleared")
	}
}

// TestCreateMembershipReplaces — re-granting an existing membership
// replaces the capability set.
func TestCreateMembershipReplaces(t *testing.T) {
	r, db := newRegistryWithSlugs(t)
	ctx := context.Background()
	mustSetupTenantAndActor(t, r, db, "tnt_a", "actor_a")

	if _, err := r.CreateMembership(ctx, Membership{
		ActorID: "actor_a", TenantID: "tnt_a",
		Capabilities: []string{"opstack:read"},
	}); err != nil {
		t.Fatalf("CreateMembership #1: %v", err)
	}
	if _, err := r.CreateMembership(ctx, Membership{
		ActorID: "actor_a", TenantID: "tnt_a",
		Capabilities: []string{"admin:all"},
	}); err != nil {
		t.Fatalf("CreateMembership #2: %v", err)
	}

	m, err := r.LoadMembership(ctx, "actor_a", "tnt_a")
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if !reflect.DeepEqual(m.Capabilities, []string{"admin:all"}) {
		t.Errorf("capabilities not replaced: got %v", m.Capabilities)
	}
}

// TestRevokeMembershipHidesFromList — once revoked, the membership
// disappears from LoadMembership and ListMembershipsForActor.
func TestRevokeMembershipHidesFromList(t *testing.T) {
	r, db := newRegistryWithSlugs(t)
	ctx := context.Background()
	mustSetupTenantAndActor(t, r, db, "tnt_a", "actor_a")
	if _, err := r.CreateMembership(ctx, Membership{
		ActorID: "actor_a", TenantID: "tnt_a", Capabilities: []string{"admin:all"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}
	if err := r.RevokeMembership(ctx, "actor_a", "tnt_a"); err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if _, err := r.LoadMembership(ctx, "actor_a", "tnt_a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadMembership after revoke: want ErrNotFound, got %v", err)
	}
	ms, err := r.ListMembershipsForActor(ctx, "actor_a")
	if err != nil {
		t.Fatalf("ListMembershipsForActor: %v", err)
	}
	if len(ms) != 0 {
		t.Errorf("expected 0 active memberships after revoke; got %d", len(ms))
	}
}

// TestListMembershipsForActorAcrossTenants — an actor in two tenants
// shows up exactly twice, with both slugs resolved via TenantLookup.
func TestListMembershipsForActorAcrossTenants(t *testing.T) {
	r, db := newRegistryWithSlugs(t)
	ctx := context.Background()
	mustSetupTenantAndActor(t, r, db, "tnt_a", "actor_a")
	seedTenant(t, db, "tnt_b", "beta")
	for _, tid := range []string{"tnt_a", "tnt_b"} {
		if _, err := r.CreateMembership(ctx, Membership{
			ActorID: "actor_a", TenantID: tid, Capabilities: []string{"admin:all"},
		}); err != nil {
			t.Fatalf("CreateMembership(%s): %v", tid, err)
		}
	}
	ms, err := r.ListMembershipsForActor(ctx, "actor_a")
	if err != nil {
		t.Fatalf("ListMembershipsForActor: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 memberships, got %d", len(ms))
	}
	slugs := []string{ms[0].TenantSlug, ms[1].TenantSlug}
	sort.Strings(slugs)
	want := []string{"beta", "tnt_a-slug"}
	if !reflect.DeepEqual(slugs, want) {
		t.Errorf("slug lookup failed: got %v, want %v", slugs, want)
	}
}

// TestListActorsInTenant — only actors with an ACTIVE membership in the
// tenant come back. A revoked actor with a live membership is still
// listed (forensics); a revoked membership is not.
func TestListActorsInTenant(t *testing.T) {
	r, db := newRegistryWithSlugs(t)
	ctx := context.Background()
	seedTenant(t, db, "tnt_a", "alpha")
	seedTenant(t, db, "tnt_b", "bravo")

	grants := map[string][]string{
		"actor_a_only":      {"tnt_a"},
		"actor_b_only":      {"tnt_b"},
		"actor_both":        {"tnt_a", "tnt_b"},
		"actor_a_revoked_m": {"tnt_a"},
		"actor_a_revoked":   {"tnt_a"},
	}
	for actorID, tids := range grants {
		if err := r.CreateActor(ctx, Actor{ActorID: actorID}); err != nil {
			t.Fatalf("CreateActor(%s): %v", actorID, err)
		}
		for _, tid := range tids {
			if _, err := r.CreateMembership(ctx, Membership{
				ActorID: actorID, TenantID: tid, Capabilities: []string{"opstack:read"},
			}); err != nil {
				t.Fatalf("CreateMembership(%s, %s): %v", actorID, tid, err)
			}
		}
	}
	if err := r.RevokeMembership(ctx, "actor_a_revoked_m", "tnt_a"); err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if err := r.RevokeActor(ctx, "actor_a_revoked"); err != nil {
		t.Fatalf("RevokeActor: %v", err)
	}

	actors, err := r.ListActorsInTenant(ctx, "tnt_a")
	if err != nil {
		t.Fatalf("ListActorsInTenant: %v", err)
	}
	var got []string
	for _, a := range actors {
		got = append(got, a.ActorID)
		if a.ActorID == "actor_a_revoked" && a.RevokedAt == nil {
			t.Errorf("actor_a_revoked listed without RevokedAt")
		}
	}
	sort.Strings(got)
	want := []string{"actor_a_only", "actor_a_revoked", "actor_both"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tnt_a actors = %v, want %v", got, want)
	}
}

// TestInvitationsScopedToTenant — ListInvitations and RevokeInvitation
// only see the given tenant's rows. Pre-0008 rows with a NULL or empty
// tenant_id count as the default tenant; another tenant's invitation is
// ErrNotFound on revoke and stays live.
func TestInvitationsScopedToTenant(t *testing.T) {
	r, db := newRegistryWithSlugs(t)
	ctx := context.Background()
	seedTenant(t, db, "tnt_a", "alpha")

	mint := func(id, tenantID string) {
		t.Helper()
		if err := r.CreateInvitation(ctx, Invitation{
			InvitationID: id,
			TokenHash:    HashToken(id),
			TenantID:     tenantID,
			Capabilities: []string{"admin:all"},
			CreatedBy:    "actor_admin",
			ExpiresAt:    time.Now().UTC().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateInvitation(%s): %v", id, err)
		}
	}
	mint("inv_a", "tnt_a")
	mint("inv_default", "") // CreateInvitation defaults to DefaultTenantID
	// Legacy rows: CreateInvitation can't produce these, so insert raw.
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for id, tenant := range map[string]any{"inv_legacy_empty": "", "inv_legacy_null": nil} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO actor_invitations
				(invitation_id, token_hash, tenant_id, capabilities, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, '["admin:all"]', 'actor_admin', '2026-01-01T00:00:00Z', ?)`,
			id, HashToken(id), tenant, expires); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	ids := func(tenantID string) []string {
		t.Helper()
		invs, err := r.ListInvitations(ctx, tenantID)
		if err != nil {
			t.Fatalf("ListInvitations(%s): %v", tenantID, err)
		}
		var out []string
		for _, inv := range invs {
			out = append(out, inv.InvitationID)
		}
		sort.Strings(out)
		return out
	}
	if got, want := ids("tnt_a"), []string{"inv_a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tnt_a invitations = %v, want %v", got, want)
	}
	if got, want := ids(DefaultTenantID), []string{"inv_default", "inv_legacy_empty", "inv_legacy_null"}; !reflect.DeepEqual(got, want) {
		t.Errorf("default invitations = %v, want %v", got, want)
	}

	// Cross-tenant revoke: not found, and the row stays live.
	if err := r.RevokeInvitation(ctx, "inv_default", "tnt_a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant revoke: got %v, want ErrNotFound", err)
	}
	inv, err := r.LookupInvitationByTokenHash(ctx, HashToken("inv_default"))
	if err != nil {
		t.Fatalf("LookupInvitationByTokenHash: %v", err)
	}
	if inv.RevokedAt != nil {
		t.Errorf("inv_default revoked by a cross-tenant call")
	}

	// In-tenant revokes succeed, including a legacy row via the default tenant.
	for _, tc := range []struct{ id, tenantID string }{
		{"inv_a", "tnt_a"},
		{"inv_legacy_null", DefaultTenantID},
	} {
		if err := r.RevokeInvitation(ctx, tc.id, tc.tenantID); err != nil {
			t.Errorf("RevokeInvitation(%s, %s): %v", tc.id, tc.tenantID, err)
		}
		inv, err := r.LookupInvitationByTokenHash(ctx, HashToken(tc.id))
		if err != nil {
			t.Fatalf("LookupInvitationByTokenHash(%s): %v", tc.id, err)
		}
		if inv.RevokedAt == nil {
			t.Errorf("%s not revoked", tc.id)
		}
	}
}

// mustSetupTenantAndActor seeds one tenant + one actor. Slug =
// <tenantID>-slug for deterministic ordering in cross-tenant checks.
func mustSetupTenantAndActor(t *testing.T, r *Registry, db *sql.DB, tenantID, actorID string) {
	t.Helper()
	seedTenant(t, db, tenantID, tenantID+"-slug")
	if err := r.CreateActor(context.Background(), Actor{ActorID: actorID, Label: actorID}); err != nil {
		t.Fatalf("CreateActor(%s): %v", actorID, err)
	}
}
