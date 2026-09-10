package admin

// Read-only operator surface over the remote-source watchers
// (GET /v1/tenants/{tenant}/sources). Lists each declared source with its
// runtime poll state — cursor, claim holder, next/last poll, last error — so an
// operator can see whether a delegated mailbox is being read and why it might be
// stuck. Declaration lives in OPS (SOURCES/ packs), so there is no add/remove
// verb here; this is a window, not an editor. No secret ever appears: the
// tenant_sources `config` carries a secret NAME, and this endpoint returns
// neither the config blob nor any value.

import (
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	chsource "github.com/loremlabs/thanks-computer/chassis/source"
)

// sourcesListResponse is the wire shape of GET /v1/tenants/{tenant}/sources.
type sourcesListResponse struct {
	Sources []chsource.Status `json:"sources"`
	Count   int               `json:"count"`
}

// handleListSources returns every source of the URL tenant (including retired),
// with runtime state. Read-only. Tenant is the SLUG (ac.TenantSlug): sources are
// keyed by slug (the poller materializes secrets and routes by slug).
func (c *Controller) handleListSources(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "source:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_slug_missing", nil)
		return
	}
	if c.srcStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "source_store_unavailable",
			map[string]any{"hint": "no runtime source store on this node"})
		return
	}
	rows, err := c.srcStore.List(r.Context(), ac.TenantSlug)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	if rows == nil {
		rows = []chsource.Status{}
	}
	writeJSON(w, http.StatusOK, sourcesListResponse{Sources: rows, Count: len(rows)})
}
