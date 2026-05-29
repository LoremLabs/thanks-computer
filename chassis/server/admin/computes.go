package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/storeresolver"
)

// maxComputeArtifactBytes caps a single uploaded compute module. Artifacts are
// small: tens of KB (Rust/TinyGo) and ~1 KB for JS/TS (dynamically linked
// against the shared Javy plugin, which is not uploaded per op); 32 MB leaves
// ample room without inviting abuse.
const maxComputeArtifactBytes = 32 << 20

// handlePutCompute stores a content-addressed compute artifact. The path
// digest must equal sha256(body) — the upload is idempotent (re-PUTting the
// same content is a harmless overwrite, since the ref IS the content). The
// engine that runs it is recorded in the artifact manifest (?engine=, default
// "wazero"). `txco apply` calls this before activating a rule that references
// the digest.
func (c *Controller) handlePutCompute(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.astore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "compute_store_unavailable", nil)
		return
	}
	vars := mux.Vars(r)
	alg, digest := vars["alg"], vars["digest"]
	if alg != "sha256" {
		writeJSONError(w, http.StatusBadRequest, "unsupported_digest_alg",
			map[string]any{"alg": alg, "supported": "sha256"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxComputeArtifactBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read_body", map[string]any{"err": err.Error()})
		return
	}
	if len(body) > maxComputeArtifactBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "artifact_too_large",
			map[string]any{"max": maxComputeArtifactBytes})
		return
	}

	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != digest {
		writeJSONError(w, http.StatusBadRequest, "digest_mismatch",
			map[string]any{"want": digest, "got": got})
		return
	}

	engine := r.URL.Query().Get("engine")
	if engine == "" {
		engine = "wazero"
	}
	manifest, _ := json.Marshal(storeresolver.Manifest{Engine: engine, Alg: alg, Digest: digest})
	ref := compute.Ref{Alg: alg, Digest: digest}
	if err := c.astore.Put(r.Context(), ref.StoreRef(), body, manifest); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_put", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": ref.String(), "alg": alg, "digest": digest, "engine": engine, "bytes": len(body),
	})
}

// handleHeadCompute reports whether a compute artifact is already present, so
// `txco apply` can skip re-uploading unchanged modules.
func (c *Controller) handleHeadCompute(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.astore == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	vars := mux.Vars(r)
	ref := compute.Ref{Alg: vars["alg"], Digest: vars["digest"]}
	ok, err := c.astore.Exists(r.Context(), ref.StoreRef())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}
