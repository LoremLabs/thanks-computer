package admin

// A tenant's own fuel budgets over the signed admin API, for `txco allowance`:
//
//	GET    /v1/tenants/{tenant}/allowances          list (with each window's status)
//	GET    /v1/tenants/{tenant}/allowances/{name}   one allowance's current window
//	PUT    /v1/tenants/{tenant}/allowances/{name}   {fuel, per} — create or replace
//	DELETE /v1/tenants/{tenant}/allowances/{name}   remove the definition
//
// Allowances are tenant KV data (chassis/allowance keeps them in reserved KV
// namespaces), so the KV capabilities gate them: kv:*:read to look, kv:*:write
// to change. Tenant owners already hold kv:*:*; capabilities are stored on a
// key at enrollment, so a new capability would miss every existing owner.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/allowance"
	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// allowanceRecord is one allowance on the wire. Remaining is absent for an
// allowance that was entered but never defined: it is metered, not limited.
type allowanceRecord struct {
	Name      string `json:"name"`
	Defined   bool   `json:"defined"`
	Fuel      int64  `json:"fuel,omitempty"`
	Per       string `json:"per"`
	Used      int64  `json:"used"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
}

type allowanceListResponse struct {
	Allowances []allowanceRecord `json:"allowances"`
	Next       string            `json:"next,omitempty"`
	Count      int               `json:"count"`
}

type setAllowanceRequest struct {
	Fuel int64  `json:"fuel"`
	Per  string `json:"per,omitempty"`
}

func recordFromStatus(st allowance.Status) allowanceRecord {
	rec := allowanceRecord{Name: st.Name, Defined: st.Defined, Per: string(st.Per), Used: st.Used}
	if st.Defined {
		rem := st.Remaining()
		rec.Fuel, rec.Remaining = st.Fuel, &rem
	}
	if !st.ResetsAt.IsZero() {
		rec.ResetsAt = st.ResetsAt.UTC().Format(time.RFC3339)
	}
	return rec
}

// allowanceScope checks the capability and resolves the tenant slug (the KV
// keys under the slug, as processor.TenantScope does) and the store.
func (c *Controller) allowanceScope(w http.ResponseWriter, r *http.Request, capability string) (string, *allowance.Store, bool) {
	if err := policy.RequireCapability(r.Context(), capability); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return "", nil, false
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_slug_missing", nil)
		return "", nil, false
	}
	if c.pu == nil || c.pu.Allowances == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "allowance_store_unavailable",
			map[string]any{"hint": "no KV backend configured (--kvstore)"})
		return "", nil, false
	}
	return ac.TenantSlug, c.pu.Allowances, true
}

func (c *Controller) handleListAllowances(w http.ResponseWriter, r *http.Request) {
	tenant, store, ok := c.allowanceScope(w, r, "kv:*:read")
	if !ok {
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	defs, next, err := store.List(r.Context(), tenant, r.URL.Query().Get("after"), limit)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "allowance_list_err", map[string]any{"err": err.Error()})
		return
	}
	out := allowanceListResponse{Allowances: []allowanceRecord{}, Next: next}
	for _, d := range defs {
		st, err := store.Status(r.Context(), tenant, d.Name)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, "allowance_status_err",
				map[string]any{"name": d.Name, "err": err.Error()})
			return
		}
		out.Allowances = append(out.Allowances, recordFromStatus(st))
	}
	out.Count = len(out.Allowances)
	writeJSON(w, http.StatusOK, out)
}

func (c *Controller) handleGetAllowance(w http.ResponseWriter, r *http.Request) {
	tenant, store, ok := c.allowanceScope(w, r, "kv:*:read")
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	if !allowance.ValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name", map[string]any{"name": name})
		return
	}
	st, err := store.Status(r.Context(), tenant, name)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "allowance_status_err", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, recordFromStatus(st))
}

func (c *Controller) handleSetAllowance(w http.ResponseWriter, r *http.Request) {
	tenant, store, ok := c.allowanceScope(w, r, "kv:*:write")
	if !ok {
		return
	}
	var req setAllowanceRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<12))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			map[string]any{"err": err.Error(), "hint": `send {"fuel": N, "per": "hour" | "day" | "month"}`})
		return
	}
	if req.Per == "" {
		req.Per = string(allowance.DefaultPeriod)
	}
	d := allowance.Def{Name: mux.Vars(r)["name"], Fuel: req.Fuel, Per: allowance.Period(req.Per)}
	if err := d.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_allowance", map[string]any{"err": err.Error()})
		return
	}
	if err := store.Set(r.Context(), tenant, d); err != nil {
		writeJSONError(w, http.StatusBadGateway, "allowance_set_err", map[string]any{"err": err.Error()})
		return
	}
	st, err := store.Status(r.Context(), tenant, d.Name)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "allowance_status_err", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, recordFromStatus(st))
}

func (c *Controller) handleDeleteAllowance(w http.ResponseWriter, r *http.Request) {
	tenant, store, ok := c.allowanceScope(w, r, "kv:*:write")
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	if !allowance.ValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_name", map[string]any{"name": name})
		return
	}
	if err := store.Delete(r.Context(), tenant, name); err != nil {
		writeJSONError(w, http.StatusBadGateway, "allowance_delete_err", map[string]any{"err": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
