package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
)

// TestTraceTenantScope covers the authorization decision behind every trace
// endpoint: tenant-scoped routes confine a tenant-owner to their slug (and
// deny non-members); flat routes require super-admin.
func TestTraceTenantScope(t *testing.T) {
	c := &Controller{}
	call := func(ac *auth.Context) (tenant string, ok bool, code int) {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		if ac != nil {
			r = r.WithContext(auth.WithContext(r.Context(), ac))
		}
		w := httptest.NewRecorder()
		tenant, ok = c.traceTenantScope(w, r)
		return tenant, ok, w.Code
	}

	t.Run("tenant owner (opstack:*:*) allowed + scoped", func(t *testing.T) {
		tn, ok, _ := call(&auth.Context{Source: "signed", TenantSlug: "prod-mankins", Capabilities: []string{"opstack:*:*"}})
		if !ok || tn != "prod-mankins" {
			t.Errorf("got (%q, %v), want (prod-mankins, true)", tn, ok)
		}
	})
	t.Run("tenant member without trace cap denied", func(t *testing.T) {
		_, ok, code := call(&auth.Context{Source: "signed", TenantSlug: "prod-mankins", Capabilities: []string{"stack:*:read"}})
		if ok || code != http.StatusForbidden {
			t.Errorf("got (ok=%v, code=%d), want (false, 403)", ok, code)
		}
	})
	t.Run("flat super-admin allowed, no filter", func(t *testing.T) {
		tn, ok, _ := call(&auth.Context{Source: "signed", SuperAdmin: true})
		if !ok || tn != "" {
			t.Errorf("got (%q, %v), want (\"\", true)", tn, ok)
		}
	})
	t.Run("flat non-super signed denied", func(t *testing.T) {
		_, ok, code := call(&auth.Context{Source: "signed", Capabilities: []string{"opstack:*:*"}})
		if ok || code != http.StatusForbidden {
			t.Errorf("got (ok=%v, code=%d), want (false, 403)", ok, code)
		}
	})
	t.Run("super-admin scoped to a tenant still scoped (no cross-tenant)", func(t *testing.T) {
		// On a tenant-scoped route, even a super-admin is confined to the
		// URL's slug (RequireCapability short-circuits for super_admin).
		tn, ok, _ := call(&auth.Context{Source: "signed", TenantSlug: "acme", SuperAdmin: true})
		if !ok || tn != "acme" {
			t.Errorf("got (%q, %v), want (acme, true)", tn, ok)
		}
	})
	t.Run("nil context denied", func(t *testing.T) {
		_, ok, code := call(nil)
		if ok || code != http.StatusForbidden {
			t.Errorf("got (ok=%v, code=%d), want (false, 403)", ok, code)
		}
	})
}
