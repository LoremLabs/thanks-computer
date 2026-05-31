package admin

import (
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// resolveTenantMiddleware is the resolver for `/v1/tenants/{tenant}/…`
// routes. It pulls the slug out of the URL, looks the tenant up,
// attaches slug + id to the request's auth.Context, AND — for signed,
// non-super-admin callers — REPLACES Capabilities with the caller's
// membership caps for THIS tenant (no membership → empty → denied).
// That re-resolution per request from actor_memberships is what
// confines a signed actor to the tenant in the URL; capabilities are
// never carried across tenants. (super_admin and basic-auth/open keep
// their synthetic admin:all for operator/emergency access.)
//
// On unknown / revoked tenants it short-circuits to 404 — the route
// never reaches the handler. We don't 403 here because revealing
// "this slug exists but you can't see it" leaks tenant inventory; a
// uniform 404 keeps directory enumeration noisy.
func (c *Controller) resolveTenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		slug := vars["tenant"]
		if slug == "" {
			// Subrouter without a {tenant} var; nothing to do. (Should
			// not happen in practice — mux enforces the variable's
			// presence — but be defensive.)
			next.ServeHTTP(w, r)
			return
		}
		tenant, err := c.tenants.LookupBySlug(r.Context(), slug)
		if err != nil {
			if errors.Is(err, tenants.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "tenant_not_found",
					map[string]any{"slug": slug})
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "tenant_lookup_failed", nil)
			return
		}
		if tenant.RevokedAt != nil {
			writeJSONError(w, http.StatusNotFound, "tenant_not_found",
				map[string]any{"slug": slug})
			return
		}

		// Stamp the resolved tenant onto the auth context that
		// middleware already attached. Mutating the pointer keeps the
		// downstream auth.FromContext calls in handlers seeing the
		// same struct.
		ac := auth.FromContext(r.Context())
		if ac == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Browser sessions are pinned to the tenant they were minted
		// for: a cookie issued under tenant A must NOT act on tenant B.
		// Unlike signed callers (whose caps are re-resolved per tenant
		// from membership below), a browser session carries a
		// bootstrap-time capability snapshot, so without this pin a
		// member of A could replay their cookie against /v1/tenants/B/…
		// and apply A's caps to B. The incoming ac.TenantID is the
		// session's tenant (set by verifyCookie); signed callers arrive
		// with it empty. Uniform 404 so a session can't enumerate or
		// probe other tenants.
		if ac.Source == "browser" && ac.TenantID != tenant.TenantID {
			writeJSONError(w, http.StatusNotFound, "tenant_not_found",
				map[string]any{"slug": slug})
			return
		}

		ac.TenantSlug = tenant.Slug
		ac.TenantID = tenant.TenantID

		// For SIGNED, non-super-admin callers, replace the chassis-wide
		// capability list (loaded from actor_capabilities in the auth
		// middleware) with this tenant's membership row. No membership
		// → empty capability list, which RequireCapability denies. The
		// super_admin and basic-auth/open branches keep their synthetic
		// admin:all so operator emergency access stays open.
		if ac.Source == "signed" && !ac.SuperAdmin {
			m, err := c.registry.LoadMembership(r.Context(), ac.ActorID, tenant.TenantID)
			switch {
			case errors.Is(err, registry.ErrNotFound):
				ac.Capabilities = nil
			case err != nil:
				writeJSONError(w, http.StatusInternalServerError,
					"membership_lookup_failed", nil)
				return
			default:
				ac.Capabilities = m.Capabilities
			}
		}
		next.ServeHTTP(w, r)
	})
}
