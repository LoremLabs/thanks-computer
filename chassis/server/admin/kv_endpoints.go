package admin

// Read-only operator surface over the op-writable KV store
// (GET /v1/tenants/{tenant}/kv/{namespace}). Lists the keys an op has
// accumulated in a namespace — e.g. a blog's `blog_subscribers` — so an operator
// can see/export the set that txco://kv/* ops build. Metadata (keys) only;
// values are fetched per-key with kv/get. Windowed: ?limit (<= MaxListLimit) and
// ?after (cursor), with `next` for the following page.

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
)

// kvListResponse is the wire shape of GET /v1/tenants/{tenant}/kv/{namespace}.
type kvListResponse struct {
	Namespace string   `json:"namespace"`
	Keys      []string `json:"keys"`
	Next      string   `json:"next,omitempty"`
	Count     int      `json:"count"`
}

// handleListKV returns a windowed page of the user keys under the URL tenant's
// {namespace}. Read-only. Tenant is the SLUG (ac.TenantSlug): the KV ops compose
// keys under the tenant slug (processor.TenantScope), NOT the numeric TenantID
// the secret store keys by — so listing must use the slug to hit the same rows.
func (c *Controller) handleListKV(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "kv:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_slug_missing", nil)
		return
	}
	if c.pu == nil || c.pu.Kv == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "kv_store_unavailable",
			map[string]any{"hint": "no KV backend configured (--kvstore)"})
		return
	}

	ns := mux.Vars(r)["namespace"]
	after := r.URL.Query().Get("after")
	limit := 0 // 0 → the store's default page size
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	// maxValue/maxTTL are irrelevant to listing (no writes); key composition is
	// config-independent, so a zero-config view over the store handle is fine.
	keys, next, err := kvstore.New(c.pu.Kv, 0, 0).
		ListKeysPage(r.Context(), ac.TenantSlug, ns, after, limit)
	if err != nil {
		// The only client-controlled input is the namespace; a bad one (or a
		// backend hiccup) surfaces its reason to the operator.
		writeJSONError(w, http.StatusBadRequest, "kv_list_err",
			map[string]any{"err": err.Error()})
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeJSON(w, http.StatusOK, kvListResponse{
		Namespace: ns, Keys: keys, Next: next, Count: len(keys),
	})
}
