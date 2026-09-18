package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// --- signing back in after an admin-UI sign-out -----------------------------
//
// POST /auth/oauth/reauth is the other half of the re-auth marker
// (registry.MarkActorReauthRequired): sign-out stamps it, the browser
// bootstrap refuses while it is set, and THIS clears it — for a machine
// that is already enrolled and whose user has just signed in through the
// identity provider again.
//
// It exists because enrollment is the wrong tool for that job. Enroll is
// how a key JOINS a tenant: given an identity with no tenant it offers to
// create one, and given an identity that owns a different tenant it adds
// the key's actor to it. A user who is only trying to get back into the
// admin UI — and may have picked the wrong account at the provider — must
// not be walked into either. So this endpoint clears the marker and does
// nothing else: it never creates a tenant, an actor, a key or a
// membership, and a mismatch is an error that says which account was used.
//
// The rule: the verified identity must own a tenant (oidc_subjects) that
// the key's actor is already a member of. That is exactly the state a
// successful enroll leaves behind, so the account that enrolled a machine
// can always sign it back in, and no other account can.
//
// Unsigned like enroll (the id_token is the credential), same throttle,
// same issuer gate. The id_token is never logged.

type oauthReauthRequest struct {
	IDToken   string `json:"id_token"`
	PublicKey string `json:"public_key"` // base64 (std or url) ed25519, 32 bytes
}

type oauthReauthResponse struct {
	ActorID    string `json:"actor_id"`
	TenantSlug string `json:"tenant_slug"`
	SignedInAs string `json:"signed_in_as"`
}

func (c *Controller) handleOAuthReauth(w http.ResponseWriter, r *http.Request) {
	if c.oauthIssuer == "" {
		writeJSONError(w, http.StatusNotFound, "not_found", map[string]any{
			"hint": "cloud OAuth sign-in is not enabled on this chassis",
		})
		return
	}

	var req oauthReauthRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	sub, err := c.verifyOAuthIDToken(r.Context(), req.IDToken)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", map[string]any{
			"hint": "id_token failed verification",
		})
		return
	}
	pubKey, err := decodeEd25519PubKey(req.PublicKey)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid public key", map[string]any{
			"err": "public_key must decode to a 32-byte ed25519 key",
		})
		return
	}

	key, err := c.registry.LookupKeyByPublicKey(r.Context(), pubKey)
	switch {
	case errors.Is(err, registry.ErrNotFound) || (err == nil && key == nil):
		writeJSONError(w, http.StatusNotFound, "key_not_enrolled", map[string]any{
			"hint": "this key is not enrolled on this chassis; run `txco login` to enroll it",
		})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_key", map[string]any{"err": err.Error()})
		return
	}

	// The account has to be one this machine was enrolled by. `signed_in_as`
	// names the account the caller USED (they just proved it is theirs) —
	// never the one expected, which is not theirs to learn.
	mismatch := func(code, hint string) {
		c.pu.Logger.Info("re-authentication refused — identity does not match the key's tenant",
			zap.String("actor", key.ActorID), zap.String("code", code))
		writeJSONError(w, http.StatusForbidden, code, map[string]any{
			"signed_in_as": sub,
			"hint":         hint,
		})
	}
	tenantID, err := c.registry.LookupOIDCSubject(r.Context(), c.oauthIssuer, sub)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		mismatch("identity_not_enrolled", "this account has no space on this chassis; sign in with the account this machine was enrolled with")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_subject", map[string]any{"err": err.Error()})
		return
	}
	m, err := c.registry.LoadMembership(r.Context(), key.ActorID, tenantID)
	switch {
	case errors.Is(err, registry.ErrNotFound) || (err == nil && m == nil):
		mismatch("identity_mismatch", "this machine's key belongs to a different account's space; sign in with the account it was enrolled with")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_membership", map[string]any{"err": err.Error()})
		return
	}

	if err := c.registry.ClearActorReauthRequired(r.Context(), key.ActorID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "clear_reauth", map[string]any{"err": err.Error()})
		return
	}
	c.pu.Logger.Info("actor re-authenticated",
		zap.String("actor", key.ActorID), zap.String("tenant", tenantID))
	writeJSON(w, http.StatusOK, oauthReauthResponse{
		ActorID:    key.ActorID,
		TenantSlug: m.TenantSlug,
		SignedInAs: sub,
	})
}
