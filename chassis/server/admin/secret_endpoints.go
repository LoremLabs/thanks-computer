package admin

// Per-tenant secret store CRUD. All eight endpoints sit under the
// tenant-scoped subrouter (/v1/tenants/{slug}/secrets/...) so
// resolveTenantMiddleware has already populated ac.TenantID and the
// capability gate runs against the caller's membership in that
// tenant.
//
// Reveal-never is a structural property of the response types:
// only `secretWithValueResponse` carries `value`, and it's used by
// exactly two endpoints — POST /generate and POST /{name}/rotate-
// generated. Every other endpoint returns plain `secretResponse` or
// `listSecretsResponse`, neither of which has a value field. There
// is no admin endpoint that reveals the cleartext of an existing
// secret; per design §5, to inspect a value, rotate it.
//
// See internal docs/todo-secret-store.md (design) and
// internal docs/todo-secret-store-implementation.md PR 4.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// defaultGeneratedByteLen is the byte length used when POST /generate
// or POST /{name}/rotate-generated omit `byte_len`. 32 bytes of
// crypto/rand entropy is the right size for an API token or HMAC key
// and base64-encodes to 43 URL-safe characters (no padding).
const defaultGeneratedByteLen = 32

// secretRecord is the metadata view returned by every endpoint that
// doesn't generate a value. The struct has NO value field; serializing
// any non-`*WithValueResponse` shape cannot leak cleartext.
type secretRecord struct {
	SecretID      string `json:"secret_id"`
	TenantID      string `json:"tenant_id"`
	Stack         string `json:"stack,omitempty"` // "" = tenant-wide
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by,omitempty"`
	LastRotatedAt string `json:"last_rotated_at,omitempty"`
	KeyVersion    int    `json:"key_version"`
	VersionNo     int    `json:"version_no"`
}

type listSecretsResponse struct {
	Secrets []secretRecord `json:"secrets"`
}

// secretResponse is the standard write response: metadata only.
// Returned by create (operator-supplied), patch-description, rotate
// (operator-supplied), and revoke (when it returns metadata —
// revoke returns 204 No Content).
type secretResponse struct {
	Secret secretRecord `json:"secret"`
}

// secretWithValueResponse is the ONLY response shape that carries a
// cleartext value. Used by exactly two endpoints: POST /generate and
// POST /{name}/rotate-generated. Both return the value exactly once
// (the operator has the call's response body; no other endpoint can
// surface it later).
type secretWithValueResponse struct {
	Secret secretRecord `json:"secret"`
	// Value is base64-url no-padding encoded random bytes (the
	// chassis-minted cleartext). Operators copy this once; the
	// chassis itself never decodes/displays it again.
	Value string `json:"value"`
}

type createSecretRequest struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Stack       string `json:"stack,omitempty"`
}

type generateSecretRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Stack       string `json:"stack,omitempty"`
	ByteLen     int    `json:"byte_len,omitempty"`
}

// updateSecretDescriptionRequest carries the SINGLE field a PATCH
// can modify. Names are immutable (design §1.7); the handler rejects
// any request whose body contains a "name" field, even though this
// struct doesn't have one — the gjson pre-check makes the rejection
// loud rather than silently ignoring.
type updateSecretDescriptionRequest struct {
	Description string `json:"description"`
}

type rotateSecretRequest struct {
	Value string `json:"value"`
}

type rotateSecretGeneratedRequest struct {
	ByteLen int `json:"byte_len,omitempty"`
}

// secretsStoreOrError returns the secrets Store the Controller's
// processor.Unit holds, or writes a 503 to w if the feature isn't
// configured (no master key at boot). Mirrors the
// `tenants/hostnames` pattern: a single preflight, then handler
// logic.
func (c *Controller) secretsStoreOrError(w http.ResponseWriter) (*secrets.Store, bool) {
	if c.pu.Secrets == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "secret_store_unavailable",
			map[string]any{"hint": "set --secret-master-key on the chassis (or TXCO_SECRET_MASTER_KEY in env)"})
		return nil, false
	}
	return c.pu.Secrets.Store(), true
}

// optStackPtr maps a JSON `stack` field to the *string the Store
// expects. Empty string and absent both mean "tenant-wide" (nil),
// matching how the resolver treats them (see the COALESCE in the
// active-name index).
func optStackPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func metadataToRecord(m *secrets.SecretMetadata) secretRecord {
	if m == nil {
		return secretRecord{}
	}
	rec := secretRecord{
		SecretID:    m.SecretID,
		TenantID:    m.TenantID,
		Name:        m.Name,
		Description: m.Description,
		CreatedAt:   m.CreatedAt.Format(time.RFC3339),
		CreatedBy:   m.CreatedBy,
		KeyVersion:  m.KeyVersion,
		VersionNo:   m.VersionNo,
	}
	if m.Stack != nil {
		rec.Stack = *m.Stack
	}
	if m.LastRotatedAt != nil {
		rec.LastRotatedAt = m.LastRotatedAt.Format(time.RFC3339)
	}
	return rec
}

// translateStoreErr maps Store sentinels to HTTP statuses. All
// Store errors that bubble up here are operator-facing; the wire
// codes match the design's expected admin UX (404 for missing,
// 409 for duplicate, 400 for invalid name).
func translateStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, secrets.ErrSecretNotFound):
		writeJSONError(w, http.StatusNotFound, "secret_not_found", nil)
	case errors.Is(err, secrets.ErrSecretExists):
		writeJSONError(w, http.StatusConflict, "secret_exists", nil)
	case errors.Is(err, secrets.ErrInvalidName):
		writeJSONError(w, http.StatusBadRequest, "invalid_name",
			map[string]any{"hint": "name must match [A-Za-z][A-Za-z0-9_]*"})
	default:
		writeJSONError(w, http.StatusInternalServerError, "secret_store_err",
			map[string]any{"err": err.Error()})
	}
}

// handleListSecrets returns active secrets for the URL's tenant.
// Includes both tenant-wide and stack-scoped rows. Metadata only.
func (c *Controller) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	metas, err := store.ListSecrets(r.Context(), ac.TenantID)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	out := listSecretsResponse{Secrets: make([]secretRecord, 0, len(metas))}
	for _, m := range metas {
		out.Secrets = append(out.Secrets, metadataToRecord(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleShowSecret returns metadata for one secret. `?stack=foo`
// selects the stack-scoped row; absent selects the tenant-wide row.
func (c *Controller) handleShowSecret(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	stack := optStackPtr(r.URL.Query().Get("stack"))
	meta, err := store.LookupSecretMetadata(r.Context(), ac.TenantID, stack, name)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretResponse{Secret: metadataToRecord(meta)})
}

// handleCreateSecret stores an operator-supplied value. NEVER
// returns the value in the response — the operator already has it.
func (c *Controller) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	var req createSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", nil)
		return
	}
	if req.Value == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_value", nil)
		return
	}
	meta, err := store.CreateSecret(r.Context(), ac.TenantID,
		optStackPtr(req.Stack), req.Name, req.Description, ac.ActorID,
		[]byte(req.Value))
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, secretResponse{Secret: metadataToRecord(meta)})
}

// handleGenerateSecret mints a fresh random value and stores it.
// Returns the value ONCE in the response body (base64-url no-pad).
// This is the only path that emits a value alongside metadata.
func (c *Controller) handleGenerateSecret(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	var req generateSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_name", nil)
		return
	}
	byteLen := req.ByteLen
	if byteLen == 0 {
		byteLen = defaultGeneratedByteLen
	}
	cleartext, meta, err := store.GenerateSecret(r.Context(), ac.TenantID,
		optStackPtr(req.Stack), req.Name, req.Description, ac.ActorID, byteLen)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(cleartext)
	secrets.Zero(cleartext) // wipe the in-memory copy now that we've encoded
	writeJSON(w, http.StatusCreated, secretWithValueResponse{
		Secret: metadataToRecord(meta),
		Value:  encoded,
	})
}

// handleUpdateSecretDescription is the ONLY write path that doesn't
// touch the value. Rejects any attempt to include `name` in the body
// (immutable per design §1.7) — but leaves other fields silently
// ignored to keep the door open for additive description-adjacent
// metadata later.
func (c *Controller) handleUpdateSecretDescription(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	stack := optStackPtr(r.URL.Query().Get("stack"))

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			map[string]any{"err": err.Error()})
		return
	}
	// Immutable-name structural assertion: reject any PATCH that
	// tries to rename. The operator's recourse is the documented
	// rename-by-create-new-plus-revoke-old workflow.
	if gjson.GetBytes(bodyBytes, "name").Exists() {
		writeJSONError(w, http.StatusBadRequest, "name_immutable",
			map[string]any{"hint": "rename = create-new + revoke-old; see docs/runbook-secret-store.md"})
		return
	}
	var req updateSecretDescriptionRequest
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body",
				map[string]any{"err": err.Error()})
			return
		}
	}
	meta, err := store.UpdateSecretDescription(r.Context(), ac.TenantID, stack, name, req.Description)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretResponse{Secret: metadataToRecord(meta)})
}

// handleRotateSecret writes a new version with operator-supplied
// cleartext. Returns metadata only.
func (c *Controller) handleRotateSecret(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	stack := optStackPtr(r.URL.Query().Get("stack"))
	var req rotateSecretRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Value == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_value", nil)
		return
	}
	meta, err := store.RotateSecret(r.Context(), ac.TenantID, stack, name, []byte(req.Value))
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, secretResponse{Secret: metadataToRecord(meta)})
}

// handleRotateSecretGenerated mints a fresh random value, stores it
// as the new active version, and returns the value ONCE (base64-url).
func (c *Controller) handleRotateSecretGenerated(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	stack := optStackPtr(r.URL.Query().Get("stack"))
	var req rotateSecretGeneratedRequest
	// Empty body is fine — defaults apply.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body",
				map[string]any{"err": err.Error()})
			return
		}
	}
	byteLen := req.ByteLen
	if byteLen == 0 {
		byteLen = defaultGeneratedByteLen
	}
	cleartext, meta, err := store.RotateSecretGenerated(r.Context(), ac.TenantID, stack, name, byteLen)
	if err != nil {
		translateStoreErr(w, err)
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString(cleartext)
	secrets.Zero(cleartext)
	writeJSON(w, http.StatusOK, secretWithValueResponse{
		Secret: metadataToRecord(meta),
		Value:  encoded,
	})
}

// handleRevokeSecret soft-deletes the active row. Returns 204 No
// Content on success — the resource is gone, there's no metadata to
// return.
func (c *Controller) handleRevokeSecret(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	store, ok := c.secretsStoreOrError(w)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	stack := optStackPtr(r.URL.Query().Get("stack"))
	if err := store.RevokeSecret(r.Context(), ac.TenantID, stack, name); err != nil {
		translateStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
