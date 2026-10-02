package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/computesrc"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// computeSourceResponse is one compute's authored source, as the admin
// shows it.
type computeSourceResponse struct {
	Digest  string            `json:"digest"`
	Stack   string            `json:"stack"`
	Version int64             `json:"version"`
	Entry   string            `json:"entry"`
	Files   []computesrc.File `json:"files"`
}

// handleGetComputeSource: GET /v1/tenants/{t}/stacks/{name}/computes/sha256/{digest}
// returns the source of a compute the stack uses — what the admin opens when
// a `compute://sha256/<digest>` is clicked.
//
// The stack's own COMPUTES/<digest>.json row is the fence: the bundle is
// found through the newest version of THIS tenant's stack that records it,
// never fetched by hash alone, so a tenant can read only source its own
// stack carries (the filecas is shared across tenants). 404 no_source when
// no version of the stack records one — applied before the chassis kept
// source, a prebuilt .wasm, or a compute the stack doesn't use.
func (c *Controller) handleGetComputeSource(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	vars := mux.Vars(r)
	name, digest := vars["name"], vars["digest"]
	path := computesrc.Path(digest)
	if computesrc.DigestFromPath(path) == "" {
		writeJSONError(w, http.StatusBadRequest, "bad_digest", map[string]any{"digest": digest})
		return
	}

	stackID, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	// Walks the stack's versions newest-first (stack_versions_by_stack_idx)
	// and probes each one's (version_id, path) primary key.
	var version int64
	var hash string
	err = c.pu.RuntimeDB.QueryRowContext(r.Context(), c.rb(`
		SELECT sv.version_number, sf.content_hash
		  FROM stack_versions sv
		  JOIN stack_files sf ON sf.version_id = sv.version_id AND sf.path = ?
		 WHERE sv.stack_id = ?
		 ORDER BY sv.version_number DESC
		 LIMIT 1`), path, stackID).Scan(&version, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "no_source", map[string]any{"stack": name, "digest": digest})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	if c.fcas == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_filecas", nil)
		return
	}
	data, err := c.fcas.Get(r.Context(), hash)
	if errors.Is(err, filecas.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "no_source", map[string]any{"stack": name, "digest": digest, "reason": "bytes_missing"})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "source_read_failed", map[string]any{"err": err.Error()})
		return
	}
	b, err := computesrc.Decode(data)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "source_invalid", map[string]any{"err": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(computeSourceResponse{
		Digest: digest, Stack: name, Version: version, Entry: b.Entry, Files: b.Files,
	})
}

// maxMissingBlobsBatch caps one POST /blobs/missing (the CLI chunks to it).
const maxMissingBlobsBatch = 1000

// handleMissingBlobs: POST /v1/tenants/{t}/blobs/missing {"hashes":[…]} →
// {"missing":[…]} — the have/want probe for many hashes in one request
// instead of a HEAD each. Same capability as the PUT it precedes. An empty
// list is a valid probe (the CLI uses it to detect this chassis release).
func (c *Controller) handleMissingBlobs(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.fcas == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_filecas", nil)
		return
	}
	var req struct {
		Hashes []string `json:"hashes"`
	}
	// 66 bytes per quoted hash plus a comma; a little headroom on top.
	r.Body = http.MaxBytesReader(w, r.Body, maxMissingBlobsBatch*70+1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
		return
	}
	if len(req.Hashes) > maxMissingBlobsBatch {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "too_many_hashes",
			map[string]any{"max": maxMissingBlobsBatch, "got": len(req.Hashes)})
		return
	}
	missing := []string{}
	seen := make(map[string]bool, len(req.Hashes))
	for _, h := range req.Hashes {
		if _, ok := filecas.ShardKey(h); !ok {
			writeJSONError(w, http.StatusBadRequest, "malformed_hash", map[string]any{"hash": h})
			return
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		ok, err := c.fcas.Exists(r.Context(), h)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "blob_check", map[string]any{"hash": h, "err": err.Error()})
			return
		}
		if !ok {
			missing = append(missing, h)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string][]string{"missing": missing})
}
