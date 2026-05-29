package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// newPrivescTestRouter wires the grant-issuing routes (POST
// /v1/tenants/{t}/auth/invitations and POST .../auth/members) behind
// a synthetic signed auth context whose Capabilities and SuperAdmin
// flags the test controls per-request via a header. The tenant
// middleware still runs, so it'll overwrite ac.Capabilities with
// the granter's membership in this tenant — which is the whole
// point of the privesc check.
func newPrivescTestRouter(t *testing.T, granterActorID string, granterIsSuperAdmin bool) (*Controller, http.Handler) {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin"})

	// Seed the granter actor so the tenant middleware has someone to
	// look up the membership for.
	if err := c.registry.CreateActor(context.Background(), registry.Actor{
		ActorID:    granterActorID,
		SuperAdmin: granterIsSuperAdmin,
	}); err != nil {
		t.Fatalf("CreateActor(%s): %v", granterActorID, err)
	}

	r := mux.NewRouter()
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source:     "signed",
				ActorID:    granterActorID,
				SuperAdmin: granterIsSuperAdmin,
			}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/auth/invitations", c.handleCreateInvitation).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/members", c.handleGrantMember).Methods(http.MethodPost)
	return c, r
}

// TestInviteRejectsPrivesc — a signed granter with only opstack:*:read
// cannot mint an invitation that grants admin:all. The 403 response
// echoes the offending capability so the caller can see what to drop.
func TestInviteRejectsPrivesc(t *testing.T) {
	c, h := newPrivescTestRouter(t, "actor_granter", false)

	// Granter has opstack:*:read in default; that satisfies the
	// actor:*:invite check via... wait, no, opstack:*:read does NOT
	// cover actor:*:invite. We need at least actor:*:invite for the
	// existing gate. Give the granter just enough to invite.
	if _, err := c.registry.CreateMembership(context.Background(), registry.Membership{
		ActorID:      "actor_granter",
		TenantID:     "tnt_default",
		Capabilities: []string{"actor:*:invite", "opstack:*:read"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"label":        "bob",
		"capabilities": []string{"admin:all"}, // privilege escalation
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/invitations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "capability_exceeds_granter") {
		t.Errorf("expected capability_exceeds_granter; got %s", rr.Body.String())
	}
	// The denied cap is echoed back in canonical form.
	if !strings.Contains(rr.Body.String(), "*:*:*") {
		t.Errorf("expected denied capability echoed; got %s", rr.Body.String())
	}
}

// TestInviteAllowsSubset — a granter with opstack:*:* can mint an
// invitation that grants opstack:*:read (a strict subset). No
// privesc; the request proceeds.
func TestInviteAllowsSubset(t *testing.T) {
	c, h := newPrivescTestRouter(t, "actor_granter", false)
	if _, err := c.registry.CreateMembership(context.Background(), registry.Membership{
		ActorID:      "actor_granter",
		TenantID:     "tnt_default",
		Capabilities: []string{"actor:*:invite", "opstack:*:*"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"label":        "viewer-bob",
		"capabilities": []string{"opstack:*:read"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/invitations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestInviteSuperAdminBypassesPrivesc — a super_admin can grant
// admin:all even though their membership row in this tenant may
// have nothing (or be missing entirely).
func TestInviteSuperAdminBypassesPrivesc(t *testing.T) {
	_, h := newPrivescTestRouter(t, "actor_root", true)
	// No membership row at all — super_admin should still pass.
	body, _ := json.Marshal(map[string]any{
		"label":        "bob",
		"capabilities": []string{"admin:all"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/invitations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("super_admin should bypass; status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestGrantMemberRejectsPrivesc — same guard on the member-CRUD
// path. A signed actor with opstack:*:* tries to upsert another
// actor's membership to admin:all; the chassis refuses.
func TestGrantMemberRejectsPrivesc(t *testing.T) {
	c, h := newPrivescTestRouter(t, "actor_granter", false)
	ctx := context.Background()
	if _, err := c.registry.CreateMembership(ctx, registry.Membership{
		ActorID: "actor_granter", TenantID: "tnt_default",
		Capabilities: []string{"actor:*:invite", "opstack:*:*"},
	}); err != nil {
		t.Fatalf("CreateMembership granter: %v", err)
	}
	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_target"}); err != nil {
		t.Fatalf("CreateActor target: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"actor_id":     "actor_target",
		"capabilities": []string{"admin:all"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "capability_exceeds_granter") {
		t.Errorf("expected capability_exceeds_granter; got %s", rr.Body.String())
	}
}

// TestGrantMemberAllowsSubset — granter with opstack:*:* upserts
// target's membership to opstack:*:read (subset). OK.
func TestGrantMemberAllowsSubset(t *testing.T) {
	c, h := newPrivescTestRouter(t, "actor_granter", false)
	ctx := context.Background()
	if _, err := c.registry.CreateMembership(ctx, registry.Membership{
		ActorID: "actor_granter", TenantID: "tnt_default",
		Capabilities: []string{"actor:*:invite", "opstack:*:*"},
	}); err != nil {
		t.Fatalf("CreateMembership granter: %v", err)
	}
	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_target"}); err != nil {
		t.Fatalf("CreateActor target: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"actor_id":     "actor_target",
		"capabilities": []string{"opstack:*:read"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
