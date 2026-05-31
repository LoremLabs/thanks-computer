package admin

// Authoritative-DNS zone preview. Renders the zone(s) the `dns` head
// would serve for the URL's tenant in standard zone-file form — the
// same ZoneSnapshot the head answers from, built on demand from the
// dbcache mirror. Read-only and listener-independent: it works whether
// or not the `dns` personality is enabled, so an operator can preview a
// zone before delegating. See internal docs/todo-dns-authority.md §6.7.

import (
	"net/http"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	dnsp "github.com/loremlabs/thanks-computer/chassis/server/personality/dns"
)

// handleDNSRender renders this tenant's served zones as a zone-file.
// `?zone=<origin>` filters to a single one (404 if it isn't one of the
// tenant's zones — no cross-tenant peek).
//
// Capability: `dns:*:read`.
func (c *Controller) handleDNSRender(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "dns:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	db := c.pu.Dbc.Snapshot()
	if db == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "mirror_unavailable", nil)
		return
	}
	snap, err := dnsp.BuildSnapshot(db, dnsp.SynthConfigFrom(c.pu.Conf), c.pu.Logger)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "dns_snapshot",
			map[string]any{"err": err.Error()})
		return
	}

	var origins []string
	if z := strings.TrimSpace(r.URL.Query().Get("zone")); z != "" {
		want := strings.ToLower(strings.TrimSuffix(z, "."))
		for _, o := range snap.OriginsForTenant(ac.TenantID) {
			if o == want {
				origins = []string{o}
				break
			}
		}
		if len(origins) == 0 {
			writeJSONError(w, http.StatusNotFound, "zone_not_found",
				map[string]any{"zone": z})
			return
		}
	} else {
		origins = snap.OriginsForTenant(ac.TenantID)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for i, o := range origins {
		if i > 0 {
			_, _ = w.Write([]byte("\n"))
		}
		if txt, ok := snap.Render(o); ok {
			_, _ = w.Write([]byte(txt))
		}
	}
}
