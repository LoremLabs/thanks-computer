package admin

// The blob plane: content-addressed byte transfer decoupled from the JSON
// draft body. `txco apply` streams each DATASETS/ artifact here (HEAD to
// probe, PUT to upload) and then references it from the draft as a
// fingerprint-only row — multi-GB artifacts never pass through the buffered
// draft-files path or the runtime DB. Generic by design: any future large
// stack_file category (the FILES/ have/want plan) can adopt the same
// endpoints unchanged.
//
// Streaming trust model: these routes mount under the StreamingBody auth
// config, so the middleware does NOT buffer the body to validate
// Content-Digest. Instead the {hash} in the URL — a covered component of
// the request signature — is the byte-level contract, and PutReader hashes
// the stream and refuses to commit on any mismatch. Nothing partial or
// unverified ever becomes visible in the CAS.

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// blobReadFloor is the minimum whole-body read budget; blobReadPerMiB adds
// streaming headroom on top (a 1 GiB PUT gets ~17 min). Generous floors, not
// throughput targets: the deadline exists so an abandoned connection can't
// hold a slot forever once the admin server's global ReadTimeout is escaped.
const (
	blobReadFloor  = 60 * time.Second
	blobReadPerMiB = time.Second
	blobReadCeil   = 2 * time.Hour
)

// handleHeadBlob: HEAD /v1/tenants/{t}/blobs/sha256/{hash} → 200 if the CAS
// holds the hash, 404 otherwise. The probe half of the have/want exchange.
func (c *Controller) handleHeadBlob(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.fcas == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	hash := mux.Vars(r)["hash"]
	if _, ok := filecas.ShardKey(hash); !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ok, err := c.fcas.Exists(r.Context(), hash)
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

// handleGetBlob: GET /v1/tenants/{t}/blobs/sha256/{hash} — streams the blob
// out of the CAS. The read half of the plane: `txco pull` fetches dataset
// artifacts (which never inline into JSON responses) through here. The
// content is what its own (signature-covered) URL names, so the client can
// — and does — verify the stream's sha256 on receipt.
func (c *Controller) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.fcas == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	hash := mux.Vars(r)["hash"]
	if _, ok := filecas.ShardKey(hash); !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rc, size, err := filecas.GetReader(r.Context(), c.fcas, hash)
	if err != nil {
		if errors.Is(err, filecas.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer rc.Close()

	// Escape the small-JSON write timeout for the streamed response, same
	// budget shape as the upload path.
	budget := blobReadFloor + time.Duration(size>>20)*blobReadPerMiB
	if budget > blobReadCeil {
		budget = blobReadCeil
	}
	rwc := http.NewResponseController(w)
	if err := rwc.SetWriteDeadline(time.Now().Add(budget)); err != nil {
		c.pu.Logger.Warn("blob get: extending write deadline failed; large downloads may hit the global timeout",
			zap.String("err", err.Error()))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handlePutBlob: PUT /v1/tenants/{t}/blobs/sha256/{hash} — the raw body is
// streamed into the CAS under {hash}. Idempotent (re-PUT of resident content
// is a success), verified (sha256 while streaming), size-capped, and never
// buffered in memory.
func (c *Controller) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if c.fcas == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "no_filecas", nil)
		return
	}
	hash := mux.Vars(r)["hash"]
	if _, ok := filecas.ShardKey(hash); !ok {
		writeJSONError(w, http.StatusBadRequest, "malformed_hash", map[string]any{"hash": hash})
		return
	}

	// Content-Length is required: the S3 backend sizes its multipart upload
	// from it, and accepting unbounded chunked bodies invites abuse.
	size := r.ContentLength
	if size < 0 {
		writeJSONError(w, http.StatusLengthRequired, "length_required",
			map[string]any{"hint": "blob uploads must send Content-Length (no chunked encoding)"})
		return
	}
	maxBytes := int64(c.pu.Conf.DatasetMaxFileBytes)
	if maxBytes > 0 && size > maxBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "blob_too_large",
			map[string]any{"size": size, "max": maxBytes})
		return
	}

	// Fail fast on a Content-Digest that contradicts the URL: both name the
	// body's sha256, so a mismatch is a client bug — cheaper to catch here
	// than after streaming gigabytes.
	if cd := r.Header.Get("Content-Digest"); cd != "" {
		if want, ok := parseSha256Digest(cd); ok && want != hash {
			writeJSONError(w, http.StatusBadRequest, "digest_url_mismatch",
				map[string]any{"content_digest": want, "url_hash": hash})
			return
		}
	}

	// Escape the admin server's global ReadTimeout (sized for small JSON
	// bodies) for this one request; the whole-body budget scales with the
	// declared size so an abandoned stream still gets reaped.
	budget := blobReadFloor + time.Duration(size>>20)*blobReadPerMiB
	if budget > blobReadCeil {
		budget = blobReadCeil
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(budget)); err != nil {
		c.pu.Logger.Warn("blob put: extending read deadline failed; large uploads may hit the global timeout",
			zap.String("err", err.Error()))
	}

	// Belt over the declared size: the body reader refuses to deliver more
	// than Content-Length promised; a shorter-than-promised body fails
	// PutReader's exact-size check instead.
	body := http.MaxBytesReader(w, r.Body, size)
	err := filecas.PutReader(r.Context(), c.fcas, hash, body, size)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"hash": hash, "size": size})
	case errors.Is(err, filecas.ErrHashMismatch):
		writeJSONError(w, http.StatusUnprocessableEntity, "hash_mismatch", map[string]any{"err": err.Error()})
	default:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "blob_too_large", map[string]any{"max": tooLarge.Limit})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "blob_store", map[string]any{"err": err.Error()})
	}
}

// parseSha256Digest extracts the lowercase hex hash from an RFC 9530
// Content-Digest value ("sha-256=:<base64>:"). ok=false for other
// algorithms or malformed values — callers treat that as "no cross-check".
func parseSha256Digest(v string) (string, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "sha-256=:")
	if !found {
		return "", false
	}
	b64, found := strings.CutSuffix(rest, ":")
	if !found {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return "", false
	}
	return hex.EncodeToString(raw), true
}
