package admin

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// invitationTTLDefault is the TTL applied when the inviter doesn't
// specify one. 24h is the sweet spot for "share via Slack today,
// teammate accepts when they get back to their desk."
const invitationTTLDefault = 24 * time.Hour

// invitationTTLMax caps how long an invitation can live. The chassis
// rejects requests that ask for more, and clamps requests that ask for
// just-too-much. 7 days keeps stale credentials from accumulating in
// chat history.
const invitationTTLMax = 7 * 24 * time.Hour

// --- create ----------------------------------------------------------------

type createInvitationRequest struct {
	Label        string   `json:"label,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	TTLSeconds   int      `json:"ttl_seconds,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"` // reserved; v1 always grants admin:all
}

type createInvitationResponse struct {
	InvitationID string `json:"invitation_id"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
}

// handleCreateInvitation mints a single-use token. Signed; requires
// actor:invite (satisfied by admin:all today). The raw token is
// returned exactly once in this response — there is no way to recover
// it later; the DB only stores its sha-256.
func (c *Controller) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:invite"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	var req createInvitationRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = invitationTTLDefault
	}
	if ttl > invitationTTLMax {
		ttl = invitationTTLMax
	}

	// Capabilities come from the request body. Empty defaults to
	// admin:all for back-compat with older CLIs that don't forward
	// the field. policy.ParseCapabilities canonicalises every entry
	// to the 3-segment form and refuses unknown strings up front so
	// a typo doesn't get persisted as a no-op role.
	caps := req.Capabilities
	if len(caps) == 0 {
		caps = []string{"admin:all"}
	}
	rawCSV := strings.Join(caps, ",")
	parsed, err := policy.ParseCapabilities(rawCSV)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "unknown_capability",
			map[string]any{"err": err.Error()})
		return
	}
	caps = parsed

	// Anti-privilege-escalation: a signed, non-super-admin granter
	// can't issue capabilities they don't have themselves in THIS
	// tenant. The tenant middleware already swapped ac.Capabilities
	// to the granter's membership row, so the check is just "does
	// any granted cap cover each requested cap?" super_admin and
	// non-signed operators bypass — same trust level as
	// RequireSuperAdmin.
	if ac := auth.FromContext(r.Context()); ac != nil && ac.Source == "signed" && !ac.SuperAdmin {
		if missing := policy.CoversAll(ac.Capabilities, caps); missing != "" {
			writeJSONError(w, http.StatusForbidden, "capability_exceeds_granter",
				map[string]any{
					"denied_capability": missing,
					"hint":              "you cannot grant capabilities you don't have yourself in this tenant",
				})
			return
		}
	}

	token, err := auth.EightWordSecret()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token", map[string]any{"err": err.Error()})
		return
	}
	invitationID := "inv_" + hxid.NewTimeSort().String()
	expiresAt := time.Now().UTC().Add(ttl)

	// When called via the tenant-scoped mux the resolveTenantMiddleware
	// has stamped TenantID onto the auth context. Pin the invitation to
	// that tenant so the consume side knows where to grant membership.
	// On the legacy flat /auth/invitations route, fall back to the
	// default tenant (CreateInvitation also defaults this) — same
	// outcome as pre-phase-5 callers that never specified one.
	tenantID := ""
	if ac := auth.FromContext(r.Context()); ac != nil {
		tenantID = ac.TenantID
	}

	inv := registry.Invitation{
		InvitationID: invitationID,
		TokenHash:    registry.HashToken(token),
		Label:        req.Label,
		Kind:         req.Kind,
		TenantID:     tenantID,
		Capabilities: caps,
		CreatedBy:    authedUser(r),
		ExpiresAt:    expiresAt,
	}
	if err := c.registry.CreateInvitation(r.Context(), inv); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create invitation",
			map[string]any{"err": err.Error()})
		return
	}

	c.pu.Logger.Info("invitation created",
		zap.String("invitation_id", invitationID),
		zap.String("created_by", inv.CreatedBy),
		zap.String("label", req.Label),
		zap.Time("expires_at", expiresAt))

	writeJSON(w, http.StatusOK, createInvitationResponse{
		InvitationID: invitationID,
		Token:        token,
		ExpiresAt:    expiresAt.Format(time.RFC3339),
	})
}

// --- list ------------------------------------------------------------------

type invitationView struct {
	InvitationID string   `json:"invitation_id"`
	Label        string   `json:"label,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Capabilities []string `json:"capabilities"`
	CreatedBy    string   `json:"created_by"`
	CreatedAt    string   `json:"created_at"`
	ExpiresAt    string   `json:"expires_at"`
	ConsumedAt   *string  `json:"consumed_at,omitempty"`
	ConsumedBy   string   `json:"consumed_by,omitempty"`
	RevokedAt    *string  `json:"revoked_at,omitempty"`
	Status       string   `json:"status"`
}

type listInvitationsResponse struct {
	Invitations []invitationView `json:"invitations"`
}

func (c *Controller) handleListInvitations(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	rows, err := c.registry.ListInvitations(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list invitations",
			map[string]any{"err": err.Error()})
		return
	}
	now := time.Now().UTC()
	out := make([]invitationView, 0, len(rows))
	for _, inv := range rows {
		v := invitationView{
			InvitationID: inv.InvitationID,
			Label:        inv.Label,
			Kind:         inv.Kind,
			Capabilities: inv.Capabilities,
			CreatedBy:    inv.CreatedBy,
			CreatedAt:    inv.CreatedAt.Format(time.RFC3339),
			ExpiresAt:    inv.ExpiresAt.Format(time.RFC3339),
			ConsumedBy:   inv.ConsumedBy,
			Status:       deriveInvitationStatus(inv, now),
		}
		if inv.ConsumedAt != nil {
			s := inv.ConsumedAt.Format(time.RFC3339)
			v.ConsumedAt = &s
		}
		if inv.RevokedAt != nil {
			s := inv.RevokedAt.Format(time.RFC3339)
			v.RevokedAt = &s
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, listInvitationsResponse{Invitations: out})
}

// deriveInvitationStatus reduces the (consumed, revoked, expired)
// triple to a single human-readable label. Active means none of the
// terminal states have fired yet.
func deriveInvitationStatus(inv registry.Invitation, now time.Time) string {
	switch {
	case inv.ConsumedAt != nil:
		return "consumed"
	case inv.RevokedAt != nil:
		return "revoked"
	case !inv.ExpiresAt.IsZero() && !inv.ExpiresAt.After(now):
		return "expired"
	default:
		return "active"
	}
}

// --- revoke ----------------------------------------------------------------

type revokeInvitationResponse struct {
	Revoked      bool   `json:"revoked"`
	InvitationID string `json:"invitation_id"`
}

func (c *Controller) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:revoke"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	invID := mux.Vars(r)["invID"]
	if invID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing invitation id", nil)
		return
	}
	if err := c.registry.RevokeInvitation(r.Context(), invID); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "invitation not found",
				map[string]any{"invitation_id": invID})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "revoke invitation",
			map[string]any{"err": err.Error()})
		return
	}
	c.pu.Logger.Info("invitation revoked", zap.String("invitation_id", invID))
	writeJSON(w, http.StatusOK, revokeInvitationResponse{Revoked: true, InvitationID: invID})
}

// --- consume ---------------------------------------------------------------

type consumeInvitationRequest struct {
	Token        string `json:"token"`
	PublicKeyB64 string `json:"public_key_b64"`
	Algorithm    string `json:"algorithm"`
	Label        string `json:"label,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

// handleConsumeInvitation is the unsigned public endpoint. Any caller
// presenting a valid (live, unexpired, unrevoked, unconsumed) token
// receives a brand-new actor + key with the capabilities the
// invitation was issued with.
//
// All rejection causes (missing token, wrong token, expired, revoked,
// already consumed) return the SAME `401 invalid_token` response.
// Two reasons for collapsing to one code:
//   - Security: an attacker probing tokens can't distinguish "wrong
//     guess" from "burned" / "expired", which would otherwise leak
//     whether a particular token shape was ever valid.
//   - Semantics: the token IS the authentication credential on an
//     otherwise-public endpoint; a bad token is fundamentally an
//     auth failure (401), not a missing-resource one (404).
func (c *Controller) handleConsumeInvitation(w http.ResponseWriter, r *http.Request) {
	var req consumeInvitationRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Algorithm == "" {
		req.Algorithm = "ed25519"
	}
	if req.Algorithm != "ed25519" {
		writeJSONError(w, http.StatusBadRequest, "unsupported algorithm",
			map[string]any{"algorithm": req.Algorithm})
		return
	}
	if req.Token == "" {
		// No token at all: same opaque 401 as a bad token. Don't
		// lead the caller toward distinguishing "missing" from
		// "wrong" — the user's CLI should already prompt for one.
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", map[string]any{
			"hint": "invitation token invalid or expired",
		})
		return
	}
	pubKey, err := base64.StdEncoding.DecodeString(req.PublicKeyB64)
	if err != nil {
		pubKey, err = base64.RawURLEncoding.DecodeString(req.PublicKeyB64)
	}
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		writeJSONError(w, http.StatusBadRequest, "invalid public key",
			map[string]any{"err": "public_key_b64 must decode to a 32-byte ed25519 key"})
		return
	}

	newActorID := "actor_" + hxid.NewTimeSort().String()
	newKeyID := "key_" + hxid.NewTimeSort().String()

	result, err := c.registry.ConsumeInvitation(r.Context(),
		registry.HashToken(req.Token), newActorID, newKeyID,
		ed25519.PublicKey(pubKey), req.Label, req.Kind)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusUnauthorized, "invalid_token", map[string]any{
				"hint": "invitation token invalid or expired",
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "consume invitation",
			map[string]any{"err": err.Error()})
		return
	}

	// result.Reused is true when the consumer's pubkey was already
	// enrolled — we bound a new membership to the EXISTING principal
	// instead of minting a duplicate. The response echoes the actor_id
	// and key_id we actually used so the CLI's meta file points at
	// the canonical identity.
	c.pu.Logger.Info("invitation consumed",
		zap.String("invitation_id", result.Invitation.InvitationID),
		zap.String("actor_id", result.ActorID),
		zap.String("key_id", result.KeyID),
		zap.String("tenant_id", result.TenantID),
		zap.String("label", req.Label),
		zap.Bool("reused_principal", result.Reused))

	// Resolve tenant slug for the response. The CLI uses it to set
	// meta.DefaultTenant; missing slug → leave empty and CLI falls
	// back to the literal "default" via ResolveTenant precedence.
	tenantSlug := ""
	if t, err := c.tenants.Lookup(r.Context(), result.TenantID); err == nil && t != nil {
		tenantSlug = t.Slug
	}

	writeJSON(w, http.StatusOK, devEnrollResponse{
		ActorID:      result.ActorID,
		KeyID:        result.KeyID,
		Capabilities: result.Invitation.Capabilities,
		TenantSlug:   tenantSlug,
	})
}
