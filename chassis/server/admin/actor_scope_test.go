package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// Tenant scoping for the actor + invitation admin routes. The
// capability check on these routes only proves authority inside the
// URL's tenant, so the handlers must not list or revoke anything that
// lives in another tenant.
//
// Fixture: tenants default + acme, and
//
//	alice   — member of default only
//	bob     — member of acme only
//	carol   — member of default AND acme
//	root    — super_admin, member of acme
//	granter — admin:all in acme (the non-super tenant admin under test)

const scopeTestActorHeader = "X-Test-Actor"

// newActorScopeRouter wires the four routes behind a synthetic signed
// auth context whose actor comes from the X-Test-Actor header (the
// super_admin flag is read from the registry, as the real middleware
// does). resolveTenantMiddleware still runs, so a non-super caller's
// caps are swapped for their membership in the URL tenant.
func newActorScopeRouter(t *testing.T) (*Controller, http.Handler) {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin"})
	ctx := context.Background()

	if err := c.tenants.Create(ctx, tenants.Tenant{TenantID: "tnt_acme", Slug: "acme"}); err != nil {
		t.Fatalf("create tenant acme: %v", err)
	}
	seed := []struct {
		actorID string
		super   bool
		tenants []string
		caps    []string
	}{
		{"actor_alice", false, []string{"tnt_default"}, []string{"opstack:*:read"}},
		{"actor_bob", false, []string{"tnt_acme"}, []string{"opstack:*:read"}},
		{"actor_carol", false, []string{"tnt_default", "tnt_acme"}, []string{"opstack:*:read"}},
		{"actor_root", true, []string{"tnt_acme"}, []string{"admin:all"}},
		{"actor_granter", false, []string{"tnt_acme"}, []string{"admin:all"}},
	}
	for _, s := range seed {
		if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: s.actorID}); err != nil {
			t.Fatalf("CreateActor(%s): %v", s.actorID, err)
		}
		if s.super {
			if err := c.registry.SetActorSuperAdmin(ctx, s.actorID, true); err != nil {
				t.Fatalf("SetActorSuperAdmin(%s): %v", s.actorID, err)
			}
		}
		for _, tid := range s.tenants {
			if _, err := c.registry.CreateMembership(ctx, registry.Membership{
				ActorID: s.actorID, TenantID: tid, Capabilities: s.caps,
			}); err != nil {
				t.Fatalf("CreateMembership(%s, %s): %v", s.actorID, tid, err)
			}
		}
	}

	r := mux.NewRouter()
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			actorID := req.Header.Get(scopeTestActorHeader)
			a, err := c.registry.LookupActor(req.Context(), actorID)
			if err != nil {
				t.Fatalf("test caller %q: %v", actorID, err)
			}
			ac := &auth.Context{Source: "signed", ActorID: actorID, SuperAdmin: a.SuperAdmin}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/auth/actors", c.handleListActors).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/actors/{actorID}/revoke", c.handleRevokeActor).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/invitations", c.handleListInvitations).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/invitations/{invID}/revoke", c.handleRevokeInvitation).Methods(http.MethodPost)
	return c, r
}

func scopeDo(t *testing.T, h http.Handler, caller, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(scopeTestActorHeader, caller)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func assertActorRevoked(t *testing.T, c *Controller, actorID string, want bool) {
	t.Helper()
	a, err := c.registry.LookupActor(context.Background(), actorID)
	if err != nil {
		t.Fatalf("LookupActor(%s): %v", actorID, err)
	}
	if got := a.RevokedAt != nil; got != want {
		t.Errorf("%s revoked=%v, want %v", actorID, got, want)
	}
}

// TestListActorsScopedToTenant — a tenant admin sees only the actors
// with a membership in the URL tenant.
func TestListActorsScopedToTenant(t *testing.T) {
	_, h := newActorScopeRouter(t)
	rr := scopeDo(t, h, "actor_granter", http.MethodGet, "/v1/tenants/acme/auth/actors")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp listActorsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, a := range resp.Actors {
		got = append(got, a.ActorID)
	}
	sort.Strings(got)
	want := []string{"actor_bob", "actor_carol", "actor_granter", "actor_root"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("acme actors = %v, want %v", got, want)
	}
}

// TestRevokeActorTenantAdminGuards — a non-super tenant admin can't
// revoke an actor outside the tenant (404), one that spans tenants
// (409), or a super_admin (403). None of those actors end up revoked.
func TestRevokeActorTenantAdminGuards(t *testing.T) {
	c, h := newActorScopeRouter(t)
	cases := []struct {
		target   string
		wantCode int
		wantBody string
	}{
		{"actor_alice", http.StatusNotFound, "actor not found"},
		{"actor_nope", http.StatusNotFound, "actor not found"},
		{"actor_carol", http.StatusConflict, "actor_spans_tenants"},
		{"actor_root", http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			rr := scopeDo(t, h, "actor_granter", http.MethodPost,
				"/v1/tenants/acme/auth/actors/"+tc.target+"/revoke")
			if rr.Code != tc.wantCode {
				t.Fatalf("status=%d, want %d; body=%s", rr.Code, tc.wantCode, rr.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rr.Body.String(), tc.wantBody) {
				t.Errorf("body=%s, want it to contain %q", rr.Body.String(), tc.wantBody)
			}
			if tc.target != "actor_nope" {
				assertActorRevoked(t, c, tc.target, false)
			}
		})
	}
	// The spanning-tenant hint must not name the other tenant.
	rr := scopeDo(t, h, "actor_granter", http.MethodPost, "/v1/tenants/acme/auth/actors/actor_carol/revoke")
	if strings.Contains(rr.Body.String(), "tnt_default") {
		t.Errorf("409 body leaks the other tenant: %s", rr.Body.String())
	}
}

// TestRevokeActorTenantAdminAllowed — an actor whose only membership is
// the admin's tenant can be revoked, and its browser sessions go too.
func TestRevokeActorTenantAdminAllowed(t *testing.T) {
	c, h := newActorScopeRouter(t)
	ctx := context.Background()
	token, _, err := c.registry.CreateBootstrap(ctx, "actor_bob", "tnt_acme",
		[]string{"opstack:*:read"}, false, "test", time.Minute)
	if err != nil {
		t.Fatalf("CreateBootstrap: %v", err)
	}
	b, err := c.registry.ConsumeBootstrap(ctx, token, "10.0.0.1")
	if err != nil {
		t.Fatalf("ConsumeBootstrap: %v", err)
	}
	sess, err := c.registry.CreateSession(ctx, b, "ua/test", "10.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := scopeDo(t, h, "actor_granter", http.MethodPost, "/v1/tenants/acme/auth/actors/actor_bob/revoke")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertActorRevoked(t, c, "actor_bob", true)
	got, err := c.registry.GetSession(ctx, sess.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.RevokedAt == nil {
		t.Errorf("bob's browser session survived the actor revoke")
	}
}

// TestRevokeActorSuperAdminBypass — a super_admin may revoke any actor
// through any tenant URL; an unknown id is still a 404.
func TestRevokeActorSuperAdminBypass(t *testing.T) {
	c, h := newActorScopeRouter(t)
	rr := scopeDo(t, h, "actor_root", http.MethodPost, "/v1/tenants/acme/auth/actors/actor_alice/revoke")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertActorRevoked(t, c, "actor_alice", true)

	rr = scopeDo(t, h, "actor_root", http.MethodPost, "/v1/tenants/acme/auth/actors/actor_nope/revoke")
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown actor: status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// TestInvitationsScopedToTenant — list shows only the URL tenant's
// invitations (legacy empty-tenant rows count as default), and revoking
// another tenant's invitation is a 404 that leaves it active.
func TestInvitationsScopedToTenant(t *testing.T) {
	c, h := newActorScopeRouter(t)
	ctx := context.Background()
	expires := time.Now().UTC().Add(time.Hour)
	for id, tid := range map[string]string{"inv_default": "tnt_default", "inv_acme": "tnt_acme"} {
		if err := c.registry.CreateInvitation(ctx, registry.Invitation{
			InvitationID: id,
			TokenHash:    registry.HashToken(id),
			TenantID:     tid,
			Capabilities: []string{"admin:all"},
			CreatedBy:    "actor_root",
			ExpiresAt:    expires,
		}); err != nil {
			t.Fatalf("CreateInvitation(%s): %v", id, err)
		}
	}
	if _, err := c.registry.DB.ExecContext(ctx,
		`INSERT INTO actor_invitations
			(invitation_id, token_hash, tenant_id, capabilities, created_by, created_at, expires_at)
		 VALUES ('inv_legacy', ?, '', '["admin:all"]', 'actor_root', '2026-01-01T00:00:00Z', ?)`,
		registry.HashToken("inv_legacy"), expires.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert legacy invitation: %v", err)
	}

	list := func(caller, slug string) map[string]string {
		t.Helper()
		rr := scopeDo(t, h, caller, http.MethodGet, "/v1/tenants/"+slug+"/auth/invitations")
		if rr.Code != http.StatusOK {
			t.Fatalf("list %s: status=%d body=%s", slug, rr.Code, rr.Body.String())
		}
		var resp listInvitationsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]string{}
		for _, inv := range resp.Invitations {
			out[inv.InvitationID] = inv.Status
		}
		return out
	}

	acme := list("actor_granter", "acme")
	if len(acme) != 1 || acme["inv_acme"] != "active" {
		t.Errorf("acme invitations = %v, want only inv_acme", acme)
	}
	def := list("actor_root", "default")
	if len(def) != 2 || def["inv_default"] == "" || def["inv_legacy"] == "" {
		t.Errorf("default invitations = %v, want inv_default + inv_legacy", def)
	}

	rr := scopeDo(t, h, "actor_granter", http.MethodPost, "/v1/tenants/acme/auth/invitations/inv_default/revoke")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke: status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if got := list("actor_root", "default")["inv_default"]; got != "active" {
		t.Errorf("inv_default status=%q after cross-tenant revoke, want active", got)
	}

	rr = scopeDo(t, h, "actor_granter", http.MethodPost, "/v1/tenants/acme/auth/invitations/inv_acme/revoke")
	if rr.Code != http.StatusOK {
		t.Fatalf("in-tenant revoke: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := list("actor_granter", "acme")["inv_acme"]; got != "revoked" {
		t.Errorf("inv_acme status=%q, want revoked", got)
	}
}
