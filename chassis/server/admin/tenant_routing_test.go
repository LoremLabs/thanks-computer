package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// newTenantTestRouter builds a stripped-down router mirroring the
// production wiring in Controller.Start, but skipping every personality
// other than admin. The auth middleware is replaced with one that
// stamps a synthetic admin:all context so handler-level routing is
// what's under test, not the signed-auth chain (which has its own
// dedicated tests).
func newTenantTestRouter(t *testing.T) (*Controller, http.Handler) {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open"})

	r := mux.NewRouter()
	r.HandleFunc("/healthz", c.handleHealth).Methods(http.MethodGet)

	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		// Synthetic admin context: every request passes the capability
		// gate. The tenant middleware below mutates the same context to
		// attach TenantSlug / TenantID.
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source:       "open",
				Capabilities: []string{"admin:all"},
			}
			ctx := auth.WithContext(req.Context(), ac)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	protected.HandleFunc("/v1/ops", c.handleListOps).Methods(http.MethodGet)
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/ops", c.handleListOps).Methods(http.MethodGet)
	return c, r
}

// TestTenantRoutingResolvesSlug — a known tenant slug routes to the
// handler and the auth context is populated.
func TestTenantRoutingResolvesSlug(t *testing.T) {
	_, h := newTenantTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/default/ops", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, ok := got["ops"]; !ok {
		t.Errorf("expected an ops array in body; got %v", got)
	}
}

// TestTenantRoutingUnknownSlug — a slug that doesn't exist in the
// tenants table returns 404 with a typed error. Doesn't leak whether
// the tenant exists-but-is-revoked vs. truly missing.
func TestTenantRoutingUnknownSlug(t *testing.T) {
	_, h := newTenantTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/no-such/ops", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d (want 404) body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "tenant_not_found") {
		t.Errorf("expected tenant_not_found in body; got %s", rr.Body.String())
	}
}

// TestTenantRoutingRevokedSlug — a tenant that exists but has
// revoked_at set must look identical to a missing one (uniform 404,
// no tenant_revoked code).
func TestTenantRoutingRevokedSlug(t *testing.T) {
	c, h := newTenantTestRouter(t)
	if err := c.tenants.Create(context.Background(), tenants.Tenant{
		TenantID: "tnt_gone", Slug: "gone",
	}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	// Soft-delete: SQL update straight to the test DB.
	if _, err := c.pu.RuntimeDB.Exec(`UPDATE tenants SET revoked_at = ? WHERE slug = 'gone'`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("soft-delete tenant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/gone/ops", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d (want 404) body=%s", rr.Code, rr.Body.String())
	}
}

// TestTenantMiddlewareSwapsCapabilities — signed actor has membership
// in tenant A but not in tenant B. The tenant middleware replaces
// ac.Capabilities with the membership's scoped set; for tenant B the
// list is emptied. A probe handler reads back what survived.
func TestTenantMiddlewareSwapsCapabilities(t *testing.T) {
	c, _ := newTenantTestRouter(t)
	ctx := context.Background()

	// Two tenants; actor has membership only in tnt_a.
	for _, tnt := range []struct{ id, slug string }{{"tnt_a", "alpha"}, {"tnt_b", "bravo"}} {
		if err := c.tenants.Create(ctx, tenants.Tenant{
			TenantID: tnt.id, Slug: tnt.slug,
		}); err != nil {
			t.Fatalf("CreateTenant %s: %v", tnt.slug, err)
		}
	}
	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_signed"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	if _, err := c.registry.CreateMembership(ctx, registry.Membership{
		ActorID: "actor_signed", TenantID: "tnt_a",
		Capabilities: []string{"opstack:read"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	// Build a probe router that synthesizes a signed (non-super) auth
	// context, then runs the tenant middleware.
	r := mux.NewRouter()
	probe := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ac := auth.FromContext(req.Context())
		w.Header().Set("X-Test-Caps", strings.Join(ac.Capabilities, ","))
		w.WriteHeader(http.StatusNoContent)
	})
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source:       "signed",
				ActorID:      "actor_signed",
				Capabilities: []string{"admin:all"}, // pretend actor_capabilities row exists
			}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/probe", probe).Methods(http.MethodGet)

	// In tenant alpha: caps replaced with the membership's
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/alpha/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if got := rr.Header().Get("X-Test-Caps"); got != "opstack:read" {
		t.Errorf("alpha: caps=%q want %q", got, "opstack:read")
	}

	// In tenant bravo: no membership → caps emptied
	req = httptest.NewRequest(http.MethodGet, "/v1/tenants/bravo/probe", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if got := rr.Header().Get("X-Test-Caps"); got != "" {
		t.Errorf("bravo: caps=%q want empty (no membership)", got)
	}
}

// TestTenantMiddlewareSuperAdminKeepsCaps — super_admin actors don't
// have their caps replaced (no membership lookup at all); they keep
// whatever the auth middleware attached. The RequireCapability /
// RequireSuperAdmin short-circuits in policy do the real work.
func TestTenantMiddlewareSuperAdminKeepsCaps(t *testing.T) {
	c, _ := newTenantTestRouter(t)

	r := mux.NewRouter()
	probe := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ac := auth.FromContext(req.Context())
		w.Header().Set("X-Test-Caps", strings.Join(ac.Capabilities, ","))
		w.Header().Set("X-Test-Super", "false")
		if ac.SuperAdmin {
			w.Header().Set("X-Test-Super", "true")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source: "signed", ActorID: "actor_root",
				SuperAdmin:   true,
				Capabilities: []string{"admin:all"},
			}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/probe", probe).Methods(http.MethodGet)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/default/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Header().Get("X-Test-Super") != "true" {
		t.Errorf("super_admin flag lost across middleware")
	}
	if rr.Header().Get("X-Test-Caps") != "admin:all" {
		t.Errorf("super_admin caps should be left alone; got %q", rr.Header().Get("X-Test-Caps"))
	}
}

// TestRequireCapabilityScopedByMembership — end-to-end: actor has
// admin:all in tenant alpha but no membership in bravo. Requests
// against /v1/tenants/alpha/ops succeed; against /v1/tenants/bravo/ops
// return 403. This is the load-bearing tenant-scoping promise that
// phase 3 makes good on.
func TestRequireCapabilityScopedByMembership(t *testing.T) {
	c, _ := newTenantTestRouter(t)
	ctx := context.Background()

	for _, tnt := range []struct{ id, slug string }{{"tnt_a", "alpha"}, {"tnt_b", "bravo"}} {
		if err := c.tenants.Create(ctx, tenants.Tenant{TenantID: tnt.id, Slug: tnt.slug}); err != nil {
			t.Fatalf("CreateTenant %s: %v", tnt.slug, err)
		}
	}
	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_alice"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	if _, err := c.registry.CreateMembership(ctx, registry.Membership{
		ActorID: "actor_alice", TenantID: "tnt_a",
		Capabilities: []string{"admin:all"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	r := mux.NewRouter()
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source:       "signed",
				ActorID:      "actor_alice",
				Capabilities: []string{"admin:all"}, // chassis-wide actor_capabilities row
			}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/ops", c.handleListOps).Methods(http.MethodGet)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/alpha/ops", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("alpha (member): want 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/tenants/bravo/ops", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("bravo (no membership): want 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "capability_denied") {
		t.Errorf("bravo: want capability_denied; got %s", rr.Body.String())
	}
}

// TestTenantContextAttached — when the slug resolves, the tenant id
// and slug are attached to auth.Context for the handler. Verified by
// a probe handler that echoes them back in a custom header.
func TestTenantContextAttached(t *testing.T) {
	c, _ := newTenantTestRouter(t)

	// Build a fresh router whose tenant-scoped handler is the probe.
	r := mux.NewRouter()
	probe := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ac := auth.FromContext(req.Context())
		if ac == nil {
			http.Error(w, "no ac", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Test-Tenant-Slug", ac.TenantSlug)
		w.Header().Set("X-Test-Tenant-ID", ac.TenantID)
		w.WriteHeader(http.StatusNoContent)
	})

	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{Source: "open", Capabilities: []string{"admin:all"}}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/probe", probe).Methods(http.MethodGet)

	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/default/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Test-Tenant-Slug"); got != "default" {
		t.Errorf("slug not attached: got %q", got)
	}
	if got := rr.Header().Get("X-Test-Tenant-ID"); got != "tnt_default" {
		t.Errorf("tenant_id not attached: got %q", got)
	}
}
