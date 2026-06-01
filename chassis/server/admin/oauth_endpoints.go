package admin

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// --- cloud OIDC-bootstrap enrollment ---------------------------------------
//
// POST /auth/oauth/enroll turns a verified OIDC id_token + an ed25519 public
// key into a tenant + actor/key + scoped membership. It is the OAuth-driven
// sibling of /auth/dev/enroll and /auth/invitations/consume: unsigned (the
// id_token is the credential), behind the same per-IP throttle, gated by
// --cloud-oauth-issuer.
//
// Scope is enrollment only — no stack is seeded. The id_token is a bearer
// secret and is never logged.

type oauthEnrollRequest struct {
	IDToken    string `json:"id_token"`
	PublicKey  string `json:"public_key"` // base64 (std or url) ed25519, 32 bytes
	Label      string `json:"label"`
	Profile    string `json:"profile"`     // CLI bookkeeping; the server ignores it
	TenantSlug string `json:"tenant_slug"` // optional; honored only on first enroll
}

type oauthEnrollResponse struct {
	ChassisURL   string   `json:"chassis_url"`
	TenantSlug   string   `json:"tenant_slug"`
	ActorID      string   `json:"actor_id"`
	KeyID        string   `json:"key_id"`
	Capabilities []string `json:"capabilities"`
}

// oauthSlugRe is a single lowercase DNS label (1–63 chars, no leading/trailing
// hyphen). Tenant slugs become part of `*.stacks.<suffix>` hostnames and the
// CLI profile's default tenant, so they must be clean labels.
var oauthSlugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// handleOAuthEnroll: verify id_token → resolve (issuer, sub) → ensure tenant
// (first enroll: the user names it via a 409+suggestion round-trip) → enroll
// the key as a tenant_owner actor → return the chassis profile fields.
func (c *Controller) handleOAuthEnroll(w http.ResponseWriter, r *http.Request) {
	if c.oauthIssuer == "" {
		writeJSONError(w, http.StatusNotFound, "not_found", map[string]any{
			"hint": "cloud OAuth enrollment is not enabled on this chassis",
		})
		return
	}

	var req oauthEnrollRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}

	// The id_token is the credential. Verify signature + iss + exp (+ aud)
	// against the configured issuer's JWKS. NEVER log the raw token.
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

	issuer := c.oauthIssuer

	tenantID, err := c.registry.LookupOIDCSubject(r.Context(), issuer, sub)
	switch {
	case err == nil:
		// Returning identity (same machine or a new key): enroll into the
		// existing tenant. Any tenant_slug in the request is ignored.
		c.oauthEnrollIntoTenant(w, r, tenantID, sub, pubKey, req.Label)
		return
	case errors.Is(err, registry.ErrNotFound):
		// First enroll — fall through to slug selection.
	default:
		writeJSONError(w, http.StatusInternalServerError, "lookup_subject", map[string]any{"err": err.Error()})
		return
	}

	// First enroll: the user names their space.
	slug := strings.ToLower(strings.TrimSpace(req.TenantSlug))
	if slug == "" {
		writeJSONError(w, http.StatusConflict, "tenant_slug_required", map[string]any{
			"suggested_tenant_slug": c.freeSlugSuggestion(r.Context(), suggestSlug(sub)),
		})
		return
	}
	if tenants.ReservedSlug(slug) || !oauthSlugRe.MatchString(slug) {
		writeJSONError(w, http.StatusConflict, "tenant_slug_invalid", map[string]any{
			"slug":                  slug,
			"suggested_tenant_slug": c.freeSlugSuggestion(r.Context(), suggestSlug(sub)),
			"hint":                  "slug must be a lowercase DNS label (a-z, 0-9, hyphen), not _-prefixed",
		})
		return
	}
	if _, err := c.tenants.LookupBySlug(r.Context(), slug); err == nil {
		writeJSONError(w, http.StatusConflict, "tenant_slug_taken", map[string]any{
			"slug":                  slug,
			"suggested_tenant_slug": c.freeSlugSuggestion(r.Context(), slug),
		})
		return
	} else if !errors.Is(err, tenants.ErrNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "lookup_slug", map[string]any{"err": err.Error()})
		return
	}

	// Create the tenant and record the (issuer, sub) → tenant mapping.
	// NOTE: single-chassis path — the auto-created tenant is not fleet-synced
	// to replicas yet (same posture as the actor/key writes the existing
	// enroll endpoints already do without fleet-sync). Multi-chassis
	// propagation is a follow-up.
	newTenantID := "tnt_" + hxid.NewTimeSort().String()
	if err := c.tenants.Create(r.Context(), tenants.Tenant{TenantID: newTenantID, Slug: slug}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create_tenant", map[string]any{"err": err.Error()})
		return
	}
	if err := c.registry.CreateOIDCSubject(r.Context(), issuer, sub, newTenantID); err != nil {
		if errors.Is(err, registry.ErrSubjectAlreadyMapped) {
			// A concurrent first-enroll for the same identity won the race.
			// Enroll into the tenant it created (ours becomes an orphan slug,
			// acceptable for v1).
			if existing, lerr := c.registry.LookupOIDCSubject(r.Context(), issuer, sub); lerr == nil {
				c.oauthEnrollIntoTenant(w, r, existing, sub, pubKey, req.Label)
				return
			}
		}
		writeJSONError(w, http.StatusInternalServerError, "map_subject", map[string]any{"err": err.Error()})
		return
	}

	c.oauthEnrollIntoTenant(w, r, newTenantID, sub, pubKey, req.Label)
}

// oauthEnrollIntoTenant enrolls (or idempotently locates) the public key as an
// actor in tenantID, grants the tenant_owner membership, and writes the
// response. Shared by the first-enroll and returning-identity paths.
func (c *Controller) oauthEnrollIntoTenant(w http.ResponseWriter, r *http.Request, tenantID, sub string, pubKey ed25519.PublicKey, label string) {
	caps := auth.TenantOwnerCaps()

	slug := ""
	if t, err := c.tenants.Lookup(r.Context(), tenantID); err == nil && t != nil {
		slug = t.Slug
	}

	// Idempotent key path: a known pubkey reuses its principal; an unknown
	// one mints a fresh actor + key (second machine / rotated key).
	var actorID, keyID string
	existing, err := c.registry.LookupKeyByPublicKey(r.Context(), pubKey)
	switch {
	case err == nil && existing != nil:
		actorID, keyID = existing.ActorID, existing.KeyID
	case err != nil && !errors.Is(err, registry.ErrNotFound):
		writeJSONError(w, http.StatusInternalServerError, "lookup_key", map[string]any{"err": err.Error()})
		return
	default:
		actorID = "actor_" + hxid.NewTimeSort().String()
		keyID = "key_" + hxid.NewTimeSort().String()
		if err := c.registry.CreateActor(r.Context(), registry.Actor{
			ActorID: actorID,
			Label:   label,
			Kind:    "user",
		}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "create actor", map[string]any{"err": err.Error()})
			return
		}
		if err := c.registry.CreateKey(r.Context(), registry.Key{
			KeyID:     keyID,
			ActorID:   actorID,
			PublicKey: pubKey,
			Algorithm: "ed25519",
		}); err != nil {
			if errors.Is(err, registry.ErrKeyAlreadyEnrolled) {
				// Race: another caller enrolled this key between our lookup
				// and insert. Re-resolve and bind a membership to it.
				if k, lerr := c.registry.LookupKeyByPublicKey(r.Context(), pubKey); lerr == nil && k != nil {
					actorID, keyID = k.ActorID, k.KeyID
				} else {
					writeJSONError(w, http.StatusConflict, "key_already_enrolled", nil)
					return
				}
			} else {
				writeJSONError(w, http.StatusInternalServerError, "create key", map[string]any{"err": err.Error()})
				return
			}
		}
	}

	if _, err := c.registry.CreateMembership(r.Context(), registry.Membership{
		ActorID:      actorID,
		TenantID:     tenantID,
		Capabilities: caps,
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create membership", map[string]any{"err": err.Error()})
		return
	}

	// id_token is intentionally NOT logged (bearer secret).
	c.pu.Logger.Info("oauth-enrolled actor",
		zap.String("subject", sub),
		zap.String("actor_id", actorID),
		zap.String("key_id", keyID),
		zap.String("tenant_id", tenantID),
		zap.String("tenant_slug", slug))

	writeJSON(w, http.StatusOK, oauthEnrollResponse{
		ChassisURL:   c.cloudChassisURL(r),
		TenantSlug:   slug,
		ActorID:      actorID,
		KeyID:        keyID,
		Capabilities: caps,
	})
}

// freeSlugSuggestion returns an available slug derived from base, appending
// -2, -3, … on collision. A blank / reserved / malformed base falls back to a
// random label. On a lookup error it returns a random label rather than
// suggest a name it couldn't verify as free.
func (c *Controller) freeSlugSuggestion(ctx context.Context, base string) string {
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" || tenants.ReservedSlug(base) || !oauthSlugRe.MatchString(base) {
		base = tenants.RandLabel()
	}
	cand := base
	for i := 2; i <= 99; i++ {
		_, err := c.tenants.LookupBySlug(ctx, cand)
		if errors.Is(err, tenants.ErrNotFound) {
			return cand
		}
		if err != nil {
			return tenants.RandLabel()
		}
		cand = fmt.Sprintf("%s-%d", base, i)
	}
	return tenants.RandLabel()
}

// suggestSlug derives a clean slug hint from an OIDC subject, subject-type
// generic: strip the `type:` prefix (email:, github:, …), take the local part
// if an email remains, then sanitize. Empty → a random label.
func suggestSlug(sub string) string {
	s := sub
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if hint := tenants.SanitizeSlugHint(s); hint != "" {
		return hint
	}
	return tenants.RandLabel()
}

// cloudChassisURL is the BASE admin URL echoed to the client (written to the
// CLI profile). Configured via --cloud-chassis-url; otherwise derived from the
// request (honoring X-Forwarded-Proto behind a TLS-terminating proxy).
func (c *Controller) cloudChassisURL(r *http.Request) string {
	if u := strings.TrimSpace(c.pu.Conf.CloudChassisURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "https"
	if r.TLS == nil {
		if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
			scheme = xf
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

// decodeEd25519PubKey accepts standard or URL-safe base64 (the CLI may use
// either) and validates the 32-byte length.
func decodeEd25519PubKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(s)
	}
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}
