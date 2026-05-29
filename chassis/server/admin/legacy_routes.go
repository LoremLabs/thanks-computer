package admin

import (
	"net/http"
	"strings"
)

// handleRouteRetired is the single 410 handler registered against
// every legacy flat path retired in phase 7. The body points at the
// tenant-scoped replacement so an operator hitting an old URL in
// `curl` immediately sees what to fix.
//
// We don't try to guess WHICH tenant — the legacy routes implicitly
// targeted the seeded "default" tenant, but new chassis deployments
// can rename or revoke that one. Pointing at the template is enough
// for the caller to do the substitution themselves.
//
// 410 (not 404) so well-behaved clients understand the route is
// permanently gone — no retry helps. The JSON body shape (`error` +
// `detail`) matches writeJSONError so callers that parse the existing
// shape see this the same way.
func (c *Controller) handleRouteRetired(w http.ResponseWriter, r *http.Request) {
	// Derive the tenant-scoped suffix from the current path so the
	// hint isn't generic. "/v1/ops/import" → "/ops/import";
	// "/auth/invitations/inv_x/revoke" → "/auth/invitations/inv_x/revoke".
	// Strip a leading "/v1" since the new shape includes that under
	// "/v1/tenants/<t>".
	suffix := r.URL.Path
	if strings.HasPrefix(suffix, "/v1/") {
		suffix = strings.TrimPrefix(suffix, "/v1")
	}
	writeJSONError(w, http.StatusGone, "route_retired", map[string]any{
		"new":  "/v1/tenants/<tenant>" + suffix,
		"hint": "admin endpoints are tenant-scoped; pass --tenant to your CLI or use /v1/tenants/<tenant>/... directly",
	})
}
