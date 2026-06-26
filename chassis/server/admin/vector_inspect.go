package admin

// Vector-store inspect + teardown admin endpoints, backing the
// `txco vector ls/show/diff/rm` CLI. These are tenant-scoped reads/mutations of
// the vector store (chassis/vector), distinct from the runtime txco://vector/*
// ops: they let an operator SEE what a stack's store-seed packs materialised and
// EXPLICITLY tear a whole collection down (the store-seed reconciler never
// auto-drops a collection — see chassis/storeseed). The store handle is wired by
// SetVectorStore; when unset the routes report it disabled.

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// vectorCollectionView is one collection's pin + item count.
type vectorCollectionView struct {
	Name           string `json:"name"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
	Dimensions     int    `json:"dimensions"`
	Metric         string `json:"metric"`
	Count          int    `json:"count"`
}

// handleListVectorCollections: GET /v1/tenants/{t}/vectors
func (c *Controller) handleListVectorCollections(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	if c.vstore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "vector_store_disabled", nil)
		return
	}
	cols, err := c.vstore.ListCollections(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_collections", map[string]any{"err": err.Error()})
		return
	}
	out := make([]vectorCollectionView, 0, len(cols))
	for _, col := range cols {
		v := vectorCollectionView{
			Name: col.Name, EmbeddingModel: col.EmbeddingModel,
			Dimensions: col.Dimensions, Metric: string(col.Metric),
		}
		// Count is an inspect convenience (catalog-sized); derive from ListIDs.
		if ids, lerr := c.vstore.ListIDs(r.Context(), ac.TenantID, col.Name); lerr == nil {
			v.Count = len(ids)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": out})
}

// handleGetVectorCollection: GET /v1/tenants/{t}/vectors/{name}
// Returns the pin + count + the item IDs (for `txco vector show`/`diff`).
// IDs are catalog-sized; an inspect endpoint, not a bulk export.
func (c *Controller) handleGetVectorCollection(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	if c.vstore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "vector_store_disabled", nil)
		return
	}
	name := mux.Vars(r)["name"]
	col, found, err := c.vstore.DescribeCollection(r.Context(), ac.TenantID, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "describe_collection", map[string]any{"err": err.Error()})
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "collection_not_found", map[string]any{"name": name})
		return
	}
	ids, err := c.vstore.ListIDs(r.Context(), ac.TenantID, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_ids", map[string]any{"err": err.Error()})
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":            col.Name,
		"embedding_model": col.EmbeddingModel,
		"dimensions":      col.Dimensions,
		"metric":          string(col.Metric),
		"count":           len(ids),
		"ids":             ids,
	})
}

// handleDropVectorCollection: DELETE /v1/tenants/{t}/vectors/{name}
// The explicit whole-collection teardown (`txco vector rm`).
func (c *Controller) handleDropVectorCollection(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	if c.vstore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "vector_store_disabled", nil)
		return
	}
	name := mux.Vars(r)["name"]
	removed, err := c.vstore.DropCollection(r.Context(), ac.TenantID, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "drop_collection", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "removed_items": removed})
}
