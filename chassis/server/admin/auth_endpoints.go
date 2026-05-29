package admin

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// --- dev enrollment ---------------------------------------------------------

type devEnrollRequest struct {
	PublicKeyB64 string `json:"public_key_b64"`
	Algorithm    string `json:"algorithm"`
	Label        string `json:"label"`
	Kind         string `json:"kind"`
}

type devEnrollResponse struct {
	ActorID      string   `json:"actor_id"`
	KeyID        string   `json:"key_id"`
	Capabilities []string `json:"capabilities"`
	// TenantSlug tells the CLI which tenant the new actor was placed
	// in (always "default" for bootstrap-local; the invitation's
	// tenant for accept). The CLI persists it into meta.DefaultTenant
	// so subsequent `txco apply` / `txco auth invite` calls find the
	// right tenant without an explicit --tenant flag.
	TenantSlug string `json:"tenant_slug,omitempty"`
	// SuperAdmin reports whether this enrolment yielded a chassis-wide
	// super-admin. True only on first-boot bootstrap.
	SuperAdmin bool `json:"super_admin,omitempty"`
}

// handleDevEnroll exchanges a shared dev secret for a new actor + key
// pair with admin:all capability. The effective secret comes from
// Controller.devEnrollSecret, which is either an operator-supplied
// --auth-dev-enroll-secret or a first-boot auto-generated 4-word
// string (see Controller.resolveDevEnrollSecret).
//
// When the secret is auto-generated, this handler enforces
// burn-after-use by re-checking the registry: if any actor already
// exists, the secret is no longer honoured — same 404 as if dev
// enrollment were disabled, so callers can't probe for a burned vs.
// never-set secret.
//
// Sharp tool: enrolment grants full admin in one shot. The startup
// WARN is the user-facing reminder.
func (c *Controller) handleDevEnroll(w http.ResponseWriter, r *http.Request) {
	if c.devEnrollSecret == "" {
		writeJSONError(w, http.StatusNotFound, "not_found", map[string]any{
			"hint": "dev enrollment is not enabled on this chassis",
		})
		return
	}
	if c.devEnrollAutoGen {
		// Burn-after-use: auto-generated secret is only valid while
		// the registry is empty. We re-query rather than maintain a
		// per-process flag so a successful enrolment in another
		// process (or this one, racily) closes the window for everyone.
		has, err := c.registry.HasAnyActiveActor(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "registry_check", map[string]any{
				"err": err.Error(),
			})
			return
		}
		if has {
			writeJSONError(w, http.StatusNotFound, "not_found", map[string]any{
				"hint": "dev enrollment is not enabled on this chassis",
			})
			return
		}
	}

	supplied := r.Header.Get("X-Txco-Enroll-Secret")
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(c.devEnrollSecret)) != 1 {
		writeJSONError(w, http.StatusUnauthorized, "invalid_enrollment_secret", nil)
		return
	}

	var req devEnrollRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	if req.Algorithm == "" {
		req.Algorithm = "ed25519"
	}
	if req.Algorithm != "ed25519" {
		writeJSONError(w, http.StatusBadRequest, "unsupported algorithm", map[string]any{"algorithm": req.Algorithm})
		return
	}
	pubKey, err := base64.StdEncoding.DecodeString(req.PublicKeyB64)
	if err != nil {
		// Allow URL-safe base64 too — the CLI may use either.
		pubKey, err = base64.RawURLEncoding.DecodeString(req.PublicKeyB64)
	}
	if err != nil || len(pubKey) != ed25519.PublicKeySize {
		writeJSONError(w, http.StatusBadRequest, "invalid public key", map[string]any{
			"err": "public_key_b64 must decode to a 32-byte ed25519 key",
		})
		return
	}

	// Phase 4 dedupe: refuse re-enrolment of a known pubkey. Without
	// this guard, the same ssh-agent key against two `--label`
	// invocations would silently create two actor rows (the original
	// bug). The 409 echoes the existing actor_id so the CLI can show
	// "already enrolled as X" and point the user at invite/accept.
	if existing, err := c.registry.LookupKeyByPublicKey(r.Context(), ed25519.PublicKey(pubKey)); err == nil && existing != nil {
		writeJSONError(w, http.StatusConflict, "key_already_enrolled", map[string]any{
			"actor_id": existing.ActorID,
			"key_id":   existing.KeyID,
			"hint":     "this public key is already enrolled; ask an admin to invite you to a tenant, or revoke the existing key first",
		})
		return
	} else if err != nil && !errors.Is(err, registry.ErrNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "lookup_key", map[string]any{"err": err.Error()})
		return
	}

	actorID := "actor_" + hxid.NewTimeSort().String()
	keyID := "key_" + hxid.NewTimeSort().String()

	if err := c.registry.CreateActor(r.Context(), registry.Actor{
		ActorID: actorID,
		Label:   req.Label,
		Kind:    req.Kind,
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create actor", map[string]any{"err": err.Error()})
		return
	}
	if err := c.registry.CreateKey(r.Context(), registry.Key{
		KeyID:     keyID,
		ActorID:   actorID,
		PublicKey: ed25519.PublicKey(pubKey),
		Algorithm: "ed25519",
	}); err != nil {
		if errors.Is(err, registry.ErrKeyAlreadyEnrolled) {
			// Race: another caller enrolled this key between our
			// lookup and our insert. Surface the same 409 shape.
			writeJSONError(w, http.StatusConflict, "key_already_enrolled", map[string]any{
				"hint": "this public key was just enrolled by another caller",
			})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "create key", map[string]any{"err": err.Error()})
		return
	}
	// First-boot bootstrap dance: when this enrolment happens via the
	// auto-generated secret on an empty registry, the actor IS the
	// chassis's super-admin. Flag them so they pass RequireSuperAdmin
	// (and so the tenant middleware doesn't bother swapping their
	// capabilities). Also grant a membership in the default tenant so
	// they show up correctly in `txco auth tenants`. Explicit-secret
	// enrolments (operator handed out a secret on purpose) do NOT get
	// the flag — those are regular admins, scoped via memberships.
	if c.devEnrollAutoGen {
		if err := c.registry.SetActorSuperAdmin(r.Context(), actorID, true); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "set super_admin", map[string]any{"err": err.Error()})
			return
		}
	}
	// Default-tenant membership: every dev-enrolment lands the actor
	// in the default tenant as an admin. This is the chassis-side
	// counterpart to the migration's backfill — without it, the
	// fresh actor can't hit `/v1/tenants/default/…` routes (no
	// membership → tenant middleware empties caps → 403). The plan
	// also asks for accept-invitation to gain its own tenant on the
	// invitation row; that lands in phase 4.
	if _, err := c.registry.CreateMembership(r.Context(), registry.Membership{
		ActorID:      actorID,
		TenantID:     tenants.DefaultTenantID,
		Capabilities: []string{"admin:all"},
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create membership", map[string]any{"err": err.Error()})
		return
	}

	c.pu.Logger.Info("dev-enrolled actor",
		zap.String("actor_id", actorID),
		zap.String("key_id", keyID),
		zap.String("label", req.Label),
		zap.String("kind", req.Kind),
		zap.Bool("super_admin", c.devEnrollAutoGen))

	writeJSON(w, http.StatusOK, devEnrollResponse{
		ActorID:      actorID,
		KeyID:        keyID,
		Capabilities: []string{"admin:all"},
		TenantSlug:   tenants.DefaultTenantSlug,
		SuperAdmin:   c.devEnrollAutoGen,
	})
}

// --- whoami ----------------------------------------------------------------

type whoamiResponse struct {
	Source       string              `json:"source"`
	ActorID      string              `json:"actor_id,omitempty"`
	KeyID        string              `json:"key_id,omitempty"`
	Label        string              `json:"label,omitempty"`
	SuperAdmin   bool                `json:"super_admin,omitempty"`
	Capabilities []string            `json:"capabilities"`
	Memberships  []whoamiMembership  `json:"memberships,omitempty"`
}

// whoamiMembership is the per-tenant slice rendered into the whoami
// response. The CLI uses this to print "you belong to X, Y, Z" without
// a second `txco auth tenants` round-trip.
type whoamiMembership struct {
	TenantID     string   `json:"tenant_id"`
	TenantSlug   string   `json:"tenant_slug"`
	Capabilities []string `json:"capabilities"`
}

// handleWhoami echoes the caller's auth context. In `both` mode
// this works for both signed callers (returns the registered actor)
// and basic-auth callers (returns the synthetic admin:all context).
// Useful during migration and after enrolment to confirm the
// chassis sees what you expect.
//
// For signed callers, also fetches the actor's Label from the
// registry so `txco auth whoami` can echo it. The label is
// purely descriptive (set at enrolment from --label or the SSH
// key comment) and isn't part of any auth decision, so a lookup
// failure just leaves the field empty rather than 500ing.
func (c *Controller) handleWhoami(w http.ResponseWriter, r *http.Request) {
	ctx := auth.FromContext(r.Context())
	if ctx == nil {
		// Open-dev mode also reaches here; surface that too.
		ctx = &auth.Context{Source: "open", Capabilities: []string{"admin:all"}}
	}
	resp := whoamiResponse{
		Source:       ctx.Source,
		ActorID:      ctx.ActorID,
		KeyID:        ctx.KeyID,
		SuperAdmin:   ctx.SuperAdmin,
		Capabilities: ctx.Capabilities,
	}
	// Memberships block: signed actors get the full set so the CLI
	// can render "you belong to X, Y" without a second query. Joined
	// to the tenants table for the slug. Basic-auth / open callers
	// have no actor row → no memberships → field omitted.
	if ctx.ActorID != "" {
		if ms, err := c.registry.ListMembershipsForActor(r.Context(), ctx.ActorID); err == nil {
			for _, m := range ms {
				resp.Memberships = append(resp.Memberships, whoamiMembership{
					TenantID:     m.TenantID,
					TenantSlug:   m.TenantSlug,
					Capabilities: m.Capabilities,
				})
			}
		}
	}
	if ctx.ActorID != "" {
		if a, err := c.registry.LookupActor(r.Context(), ctx.ActorID); err == nil && a != nil {
			resp.Label = a.Label
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- actor listing ---------------------------------------------------------

type listActorsResponse struct {
	Actors []actorRecord `json:"actors"`
}

type actorRecord struct {
	ActorID   string  `json:"actor_id"`
	Label     string  `json:"label,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	Subject   string  `json:"subject,omitempty"`
	Tenant    string  `json:"tenant,omitempty"`
	Stack     string  `json:"stack,omitempty"`
	CreatedAt string  `json:"created_at"`
	RevokedAt *string `json:"revoked_at,omitempty"`
}

func (c *Controller) handleListActors(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	actors, err := c.registry.ListActors(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list actors", map[string]any{"err": err.Error()})
		return
	}
	out := make([]actorRecord, 0, len(actors))
	for _, a := range actors {
		rec := actorRecord{
			ActorID:   a.ActorID,
			Label:     a.Label,
			Kind:      a.Kind,
			Subject:   a.Subject,
			Tenant:    a.Tenant,
			Stack:     a.Stack,
			CreatedAt: a.CreatedAt.Format(time.RFC3339),
		}
		if a.RevokedAt != nil {
			s := a.RevokedAt.Format(time.RFC3339)
			rec.RevokedAt = &s
		}
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, listActorsResponse{Actors: out})
}

// --- revocation -------------------------------------------------------------

type revokeResponse struct {
	Revoked   bool   `json:"revoked"`
	ActorID   string `json:"actor_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
}

func (c *Controller) handleRevokeActor(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:revoke"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	actorID := mux.Vars(r)["actorID"]
	if actorID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing actor id", nil)
		return
	}
	if err := c.registry.RevokeActor(r.Context(), actorID); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "actor not found", map[string]any{"actor_id": actorID})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "revoke actor", map[string]any{"err": err.Error()})
		return
	}
	c.pu.Logger.Info("actor revoked", zap.String("actor_id", actorID))
	writeJSON(w, http.StatusOK, revokeResponse{Revoked: true, ActorID: actorID})
}

// handleRevokeKey revokes a single key. Chassis-wide (the key isn't
// scoped to any tenant), so the gate is one of:
//   - super_admin (chassis-wide power)
//   - basic-auth / open operator (the same trust level)
//   - the key's owner (self-service rotation)
//
// Previously this endpoint required actor:*:revoke from the chassis-
// wide actor_capabilities table. Phase 8b retired that table; key
// revocation now uses ownership + super_admin instead, which matches
// the underlying semantics better: keys belong to actors, not to
// tenants, and an actor should always be able to revoke their own.
func (c *Controller) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	keyID := mux.Vars(r)["keyID"]
	if keyID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing key id", nil)
		return
	}

	ac := auth.FromContext(r.Context())
	if ac == nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	// super_admin + non-signed operator: always allowed. Signed
	// non-super actors must own the key they're revoking.
	if ac.Source == "signed" && !ac.SuperAdmin {
		k, err := c.registry.LookupKey(r.Context(), keyID)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "key not found", map[string]any{"key_id": keyID})
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "lookup key", map[string]any{"err": err.Error()})
			return
		}
		if k.ActorID != ac.ActorID {
			// Don't leak whether the key exists — same 403 as a
			// capability denial. Self-service is for your own keys.
			auth.WriteForbidden(w, signature.ErrCapabilityDenied)
			return
		}
	}

	if err := c.registry.RevokeKey(r.Context(), keyID); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "key not found", map[string]any{"key_id": keyID})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "revoke key", map[string]any{"err": err.Error()})
		return
	}
	c.pu.Logger.Info("key revoked", zap.String("key_id", keyID))
	writeJSON(w, http.StatusOK, revokeResponse{Revoked: true, KeyID: keyID})
}
