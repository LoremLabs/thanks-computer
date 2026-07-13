package admin

// Hostname → tenant routing CRUD. The data-plane resolver reads these
// rows on every HTTP request whose Host header misses the YAML
// ingress map; admin mutations write to runtime.db and synchronously
// reload the dbcache mirror so the next request sees the new mapping.
//
// All three endpoints sit under the tenant-scoped subrouter
// (/v1/tenants/{slug}/...) so resolveTenantMiddleware has already
// populated ac.TenantID and the capability gate runs against the
// caller's membership in that tenant.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

type hostnameRecord struct {
	ID         string `json:"id"`
	Hostname   string `json:"hostname"`
	TenantID   string `json:"tenant_id"`
	Stack      string `json:"stack"`
	CreatedAt  string `json:"created_at"`
	CreatedBy  string `json:"created_by,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

type listHostnamesResponse struct {
	Hostnames []hostnameRecord `json:"hostnames"`
}

type createHostnameRequest struct {
	Hostname string `json:"hostname"`
	Stack    string `json:"stack"`
}

// handleListHostnames returns the active hostnames bound to the URL's
// tenant. Capability `actor:*:read` because hostname listings can
// reveal a deployment's routing surface; granting "see who routes
// through this tenant" is the read-tier permission.
//
// `?history=true` flips to all-rows mode (including revoked) for
// "who used to own X" debugging — same capability gate, since the
// information is no more sensitive than the active listing.
func (c *Controller) handleListHostnames(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	includeRevoked := r.URL.Query().Get("history") == "true"
	rows, err := c.tenants.ListHostnames(r.Context(), ac.TenantID, includeRevoked)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_hostnames",
			map[string]any{"err": err.Error()})
		return
	}
	out := listHostnamesResponse{Hostnames: make([]hostnameRecord, 0, len(rows))}
	for _, h := range rows {
		out.Hostnames = append(out.Hostnames, hostnameToRecord(h))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateHostname claims a hostname for the URL's tenant against
// a specific stack. Capability `actor:*:invite` — same trust level as
// granting a membership: both alter routing/access topology and should
// require admin-tier access in the tenant.
//
// Validation order:
//  1. JSON shape (decode + required fields).
//  2. Canonicalize + IsValidHostname (strict on the admin write path).
//  3. Stack exists for this tenant.
//  4. INSERT (partial unique index on hostname WHERE revoked_at IS NULL
//     surfaces collisions as 409 with the existing owner's slug).
//  5. Reload the dbcache mirror so the data plane sees the new row.
func (c *Controller) handleCreateHostname(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	var req createHostnameRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	canon, ok := tenants.CanonicalizeHost(req.Hostname)
	if !ok || !tenants.IsValidHostname(canon) {
		writeJSONError(w, http.StatusBadRequest, "invalid_hostname",
			map[string]any{"hostname": req.Hostname})
		return
	}

	// Stack is OPTIONAL — a tenant can claim a hostname without
	// binding it to a stack and attach later via the /attach
	// endpoint (the Vercel model). When stack IS provided, it's
	// validated against the tenant's stacks here so we never land a
	// row pointing at nothing.
	if req.Stack != "" {
		_, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, req.Stack)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSONError(w, http.StatusBadRequest, "stack_not_found",
					map[string]any{"stack": req.Stack})
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "lookup_stack",
				map[string]any{"err": err.Error()})
			return
		}
	}

	h := tenants.Hostname{
		ID:        tenants.NewHostnameID(),
		Hostname:  canon,
		TenantID:  ac.TenantID,
		Stack:     req.Stack,
		CreatedAt: time.Now().UTC(),
		CreatedBy: ac.ActorID,
	}
	// Dev-local hostname auto-verify (default on; flip off on shared
	// deployments). For hostnames an operator self-evidently owns on a
	// developer machine (localhost, *.localhost, *.local,
	// *.local.thanks.computer — see tenants.IsDevLocalHostname for the
	// full list), stamp verified_at at insert time so the every-feature-
	// smoke loop doesn't need a DNS-TXT round-trip. Surfaces to the CLI
	// via the existing verified_at field in the response; the CLI then
	// skips the auto-challenge + DNS-record instructions when
	// verified_at is set.
	if c.pu.Conf.DevAutoVerifyLocalHostnames && tenants.IsDevLocalHostname(canon) {
		now := h.CreatedAt
		h.VerifiedAt = &now
	}
	// Zone-covered auto-verify: a hostname inside a zone THIS tenant has
	// delegated to us needs no separate ownership proof — the NS delegation IS
	// the proof (the same basis fromDomainVerified uses for sending), and the
	// zone already synthesizes the A/AAAA, so there's no routing record to add.
	// Stamp verified_at so the CLI skips the spurious dns-txt challenge +
	// routing-record reminder. Not flag-gated: it's a soundness fact, not a dev
	// convenience.
	if h.VerifiedAt == nil {
		if covered, cerr := tenants.DomainCoveredByZone(r.Context(), c.pu.RuntimeDB, ac.TenantSlug, canon, c.pu.RuntimeDialect); cerr == nil && covered {
			now := h.CreatedAt
			h.VerifiedAt = &now
		}
	}

	// Fleet-sync producer: upload artifact BEFORE the tx so an
	// orphaned upload (commit fails) is GC-recoverable.
	var fleetArtifactRef, fleetChecksum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadHostnameUpsert(r.Context(), h)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": ferr.Error()})
			return
		}
		fleetArtifactRef, fleetChecksum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx",
			map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := c.tenants.CreateHostnameTx(r.Context(), tx, h); err != nil {
		if errors.Is(err, tenants.ErrHostnameInUse) {
			// Roll back the in-flight tx and surface the existing
			// owner so the operator gets a useful 409. The lookup runs
			// against the bare DB (read-only), not the rolled-back tx.
			_ = tx.Rollback()
			committed = true // prevent the defer from rolling back again
			ownerSlug := ""
			if existing, lErr := c.tenants.LookupActiveHostname(r.Context(), canon); lErr == nil {
				if t, _ := c.tenants.Lookup(r.Context(), existing.TenantID); t != nil {
					ownerSlug = t.Slug
				} else {
					ownerSlug = existing.TenantID
				}
			}
			writeJSONError(w, http.StatusConflict, "hostname_owned",
				map[string]any{"hostname": canon, "tenant": ownerSlug})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "create_hostname",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeHostnameBound, ac.TenantID, "", 0, 0,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit",
			map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// Refresh the dbcache so the resolver sees this hostname:
	// synchronous on the SQLite runtime, background-coalesced on shared
	// Postgres — see Dbc.ReloadAfterWrite for why blocking the response
	// on a full mirror rebuild is wrong there.
	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after hostname create failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	writeJSON(w, http.StatusCreated, hostnameToRecord(h))
}

type mintHostnameRequest struct {
	Stack string `json:"stack"`
}

// handleMintHostname mints a structured (auto-generated) hostname bound to a
// stack and makes it routable fleet-wide — the same host the activate flow mints
// for a web stack, but ON DEMAND and for ANY stack, including a mail-only stack
// (a `_mail` channel with no web). It gives a non-web stack a reachable,
// verified, DKIM-signing host without inventing an inbox/web stack:
//
//	POST /v1/tenants/{tenant}/hostnames/mint   {"stack":"autoreply"}
//
// The host binds to the BASE stack name; mail to <localpart>@<host> then routes
// to <stack>/_mail. If the tenant has a delegated DNS zone the host is wired
// under it (<label>.<origin>); otherwise the global structured-host suffix is
// used. Verified + DKIM are set at mint (EnsureZone/SystemHostnameTx), so the
// host can both receive mail and send DKIM-signed replies immediately.
func (c *Controller) handleMintHostname(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	var req mintHostnameRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	stack := strings.Trim(strings.TrimSpace(req.Stack), "/")
	if stack == "" {
		writeJSONError(w, http.StatusBadRequest, "stack_required", nil)
		return
	}
	// The host binds to the BASE stack, but a mail-only deployment only has the
	// `<stack>/_mail` stack (no web `<stack>`). Accept either as proof the stack
	// is real, so we never mint a host that routes to nothing.
	if !c.stackOrMailChannelExists(r.Context(), ac.TenantID, stack) {
		writeJSONError(w, http.StatusBadRequest, "stack_not_found",
			map[string]any{"stack": stack, "hint": "apply the stack (or its _mail channel) first"})
		return
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	var host string
	origin, hasZone, zerr := tenants.ActivePatternZoneOriginTx(r.Context(), tx, ac.TenantID, c.pu.RuntimeDialect)
	if zerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "zone_lookup", map[string]any{"err": zerr.Error()})
		return
	}
	switch {
	case hasZone:
		host, err = tenants.EnsureZoneHostnameTx(r.Context(), tx, ac.TenantID, stack, origin, now, c.pu.RuntimeDialect)
	case c.pu.Conf.StructuredHostSuffix != "":
		host, err = tenants.EnsureSystemHostnameTx(r.Context(), tx, ac.TenantID, stack, c.pu.Conf.StructuredHostSuffix, now, c.pu.RuntimeDialect)
	default:
		writeJSONError(w, http.StatusBadRequest, "no_host_scheme",
			map[string]any{"hint": "no delegated DNS zone and no --structured-host-suffix configured"})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint", map[string]any{"err": err.Error()})
		return
	}
	if host == "" {
		writeJSONError(w, http.StatusInternalServerError, "mint", map[string]any{"err": "mint produced no hostname"})
		return
	}

	// Fleet-publish so every data-plane node routes the host (zone + structured
	// rows aren't re-derivable from a stack.activated replay). Each is a no-op
	// when its row set is empty, so calling both is safe.
	if qerr := c.queueZoneHostnameUpserts(r.Context(), tx, ac.TenantID, stack); qerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "fleet_zone_hostname", map[string]any{"err": qerr.Error()})
		return
	}
	if qerr := c.queueStructuredHostnameUpserts(r.Context(), tx, ac.TenantID, stack); qerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "fleet_structured_hostname", map[string]any{"err": qerr.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after hostname mint failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"hostname": host,
		"stack":    stack,
		"url":      structuredURL(r, host, c.pu.Conf.WebAddr),
	})
}

// stackOrMailChannelExists reports whether <stack> or <stack>/_mail has a row in
// the stacks table for the tenant — proof the base is real before binding a host.
func (c *Controller) stackOrMailChannelExists(ctx context.Context, tenantID, stack string) bool {
	if _, _, err := c.lookupStack(ctx, c.pu.RuntimeDB, tenantID, stack); err == nil {
		return true
	}
	if _, _, err := c.lookupStack(ctx, c.pu.RuntimeDB, tenantID, stack+"/_mail"); err == nil {
		return true
	}
	return false
}

// handleRevokeHostname soft-deletes the active row for the URL's
// hostname. Idempotent — revoking an absent/already-revoked hostname
// returns 200. Same capability gate as create.
func (c *Controller) handleRevokeHostname(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	hostname := mux.Vars(r)["hostname"]

	// Confirm the row currently belongs to this tenant before
	// revoking — operators shouldn't be able to release another
	// tenant's hostname via their own subrouter. A miss is fine
	// (idempotent); cross-tenant ownership is a 403.
	existing, err := c.tenants.LookupActiveHostname(r.Context(), hostname)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "hostname": hostname})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	if existing.TenantID != ac.TenantID {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	// Build the artifact for fleet sync: the existing row with
	// revoked_at stamped to "now". INSERT OR REPLACE on the consumer
	// side targets the PK (id), so this rewrites the row in place
	// with the soft-delete timestamp populated.
	revokedNow := time.Now().UTC()
	postRevoke := existing
	postRevoke.RevokedAt = &revokedNow

	var fleetArtifactRef, fleetChecksum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadHostnameUpsert(r.Context(), postRevoke)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": ferr.Error()})
			return
		}
		fleetArtifactRef, fleetChecksum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx",
			map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, _, err := c.tenants.RevokeHostnameTx(r.Context(), tx, hostname); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revoke_hostname",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeHostnameRevoked, ac.TenantID, "", 0, 0,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit",
			map[string]any{"err": err.Error()})
		return
	}
	committed = true

	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after hostname revoke failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revoked":  true,
		"hostname": existing.Hostname,
	})
}

// --- Attachment endpoint --------------------------------------------

type attachHostnameRequest struct {
	Stack string `json:"stack"`
}

// handleAttachHostname binds an existing hostname row to a specific
// stack. The hostname must already exist in this tenant (claimed
// earlier via POST /hostnames); the stack must exist in this tenant.
// Routing only kicks in once both verified_at and a non-empty stack
// are set (the resolver's JOIN enforces both). Re-attaching to a
// different stack just overwrites; no separate "swap" verb needed.
func (c *Controller) handleAttachHostname(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	hostname := mux.Vars(r)["hostname"]

	var req attachHostnameRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	if strings.TrimSpace(req.Stack) == "" {
		writeJSONError(w, http.StatusBadRequest, "stack_required", nil)
		return
	}

	// Cross-tenant ownership guard (404 — don't leak existence).
	host, err := c.tenants.LookupActiveHostname(r.Context(), hostname)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	if host.TenantID != ac.TenantID {
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	}

	// Stack must exist in this tenant.
	if _, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, req.Stack); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusBadRequest, "stack_not_found",
				map[string]any{"stack": req.Stack})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack",
			map[string]any{"err": err.Error()})
		return
	}

	// Artifact reflects the post-attach state: existing row + new stack.
	postAttach := host
	postAttach.Stack = req.Stack

	var fleetArtifactRef, fleetChecksum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadHostnameUpsert(r.Context(), postAttach)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": ferr.Error()})
			return
		}
		fleetArtifactRef, fleetChecksum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx",
			map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := c.tenants.AttachHostnameTx(r.Context(), tx, host.Hostname, req.Stack); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "attach_hostname",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		// hostname.bound is the existing event type for "hostname
		// row now points at this stack" (a generalization of the
		// original create-then-attach split). Reusing it here keeps
		// the event-type set small; the row diff in the artifact is
		// authoritative.
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeHostnameBound, ac.TenantID, "", 0, 0,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit",
			map[string]any{"err": err.Error()})
		return
	}
	committed = true

	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after attach failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	// Echo the row in its post-attach state.
	updated, err := c.tenants.LookupActiveHostname(r.Context(), host.Hostname)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, hostnameToRecord(updated))
}

// --- Verification endpoints -----------------------------------------

type createChallengeRequest struct {
	Method string `json:"method"`
	// Force, when true, rotates the active challenge token (revoke
	// prior + insert new) even if a non-expired one already exists.
	// Default (false) makes the endpoint idempotent: an existing
	// active+unexpired challenge is RETURNED instead of rotated, so a
	// retry never invalidates the TXT record the operator already
	// pasted into DNS. See internal docs/todo-custom-domains.md §6a.
	Force bool `json:"force,omitempty"`
}

type challengeRecord struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
	Instructions string `json:"instructions"`
	// Reused is true when the server returned a pre-existing active
	// challenge instead of minting a new one (idempotent path). The
	// CLI uses this to print a "reusing active challenge" note rather
	// than implying a rotation.
	Reused bool `json:"reused,omitempty"`
	// Rotated is true when a prior active challenge was revoked to
	// mint this one (only possible via force=true). The CLI uses this
	// to print a loud "previous token revoked — update your DNS" warn.
	Rotated bool `json:"rotated,omitempty"`
}

type verifyResponse struct {
	VerifiedAt string `json:"verified_at,omitempty"`
	Method     string `json:"method,omitempty"`
	Error      string `json:"error,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// handleCreateChallenge issues a challenge for the URL hostname.
//
// Idempotent by default (force=false): if a non-expired active
// challenge already exists for (hostname, method), the existing row
// is RETURNED — same token, no rotation — so a retry (or a tab that
// forgot it already ran add) doesn't invalidate the TXT record the
// operator already published. Response carries reused=true and
// 200 OK to distinguish from a fresh mint.
//
// force=true rotates: soft-revokes any prior active challenge for
// the same (hostname, method) and inserts a new one. Response
// carries rotated=true and 201 Created. Operators must update DNS.
// (The partial unique index on tenant_hostname_challenges keeps the
// state consistent across the revoke-then-insert.)
//
// Expired active rows always fall through to a fresh mint regardless
// of force (no useful reuse there). Verified rows are never reused
// — once verified, the (hostname, method) pair is closed.
//
// See internal docs/todo-custom-domains.md §6a for the operator-UX rationale.
func (c *Controller) handleCreateChallenge(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	hostname := mux.Vars(r)["hostname"]

	var req createChallengeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	if req.Method != "dns-txt" && req.Method != "http-01" {
		writeJSONError(w, http.StatusBadRequest, "invalid_method",
			map[string]any{"got": req.Method, "want": []string{"dns-txt", "http-01"}})
		return
	}

	// Cross-tenant ownership guard (404, per the discipline memory).
	host, err := c.tenants.LookupActiveHostname(r.Context(), hostname)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	if host.TenantID != ac.TenantID {
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	}

	// Idempotent path: reuse a non-expired active challenge unless the
	// caller explicitly asked to rotate. `ActiveChallenge` returns
	// `(row, ErrChallengeExpired)` for an aged-out row → we treat that
	// as "no usable active" and fall through to mint a fresh one (the
	// existing row will be revoked by `CreateChallenge`).
	if !req.Force {
		existing, lookupErr := c.tenants.ActiveChallenge(r.Context(), host.ID, req.Method)
		switch {
		case lookupErr == nil && existing != nil:
			rec := buildChallengeRecord(*existing, host.Hostname)
			rec.Reused = true
			writeJSON(w, http.StatusOK, rec)
			return
		case errors.Is(lookupErr, tenants.ErrChallengeExpired):
			// fall through — mint a new one (CreateChallenge will
			// revoke the expired prior row inside the same tx).
		case errors.Is(lookupErr, tenants.ErrNotFound):
			// no active row — fall through to mint.
		case lookupErr != nil:
			writeJSONError(w, http.StatusInternalServerError, "lookup_active_challenge",
				map[string]any{"err": lookupErr.Error()})
			return
		}
	}

	// Track whether THIS request actually revokes a prior token so we
	// can flag `rotated=true` to the operator (force=true with a real
	// prior; not a first-time mint and not an expired-row pass-through
	// — those don't surprise anyone). Cheap pre-check; the actual
	// revoke happens atomically inside CreateChallenge below.
	rotated := false
	if req.Force {
		if prev, prevErr := c.tenants.ActiveChallenge(r.Context(), host.ID, req.Method); prevErr == nil && prev != nil {
			rotated = true
		}
	}

	token, err := newChallengeToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_gen",
			map[string]any{"err": err.Error()})
		return
	}

	ch, err := c.tenants.CreateChallenge(r.Context(), host.ID, req.Method, ac.ActorID, token)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create_challenge",
			map[string]any{"err": err.Error()})
		return
	}

	rec := buildChallengeRecord(ch, host.Hostname)
	rec.Rotated = rotated
	writeJSON(w, http.StatusCreated, rec)
}

// buildChallengeRecord shapes the wire record for a stored Challenge,
// including the operator-facing instructions string. Shared between
// the "reused" (200) and "fresh mint" (201) branches of
// handleCreateChallenge so the CLI sees an identical schema regardless
// of which path the server took.
func buildChallengeRecord(ch tenants.Challenge, hostname string) challengeRecord {
	rec := challengeRecord{
		ID:        ch.ID,
		Method:    ch.Method,
		Token:     ch.Token,
		ExpiresAt: ch.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	switch ch.Method {
	case "dns-txt":
		rec.Instructions = fmt.Sprintf(
			`Add this TXT record to your DNS:
    _txco-verify.%s.  TXT  "txco-verify=%s"
Then run: txco auth tenant hostnames verify %s`,
			hostname, ch.Token, hostname)
	case "http-01":
		rec.Instructions = fmt.Sprintf(
			`Point %s at this chassis (or confirm DNS is already set),
then run: txco auth tenant hostnames verify %s
The chassis will fetch http://%s/.well-known/txco-verify/%s
and expect the body to equal %q.`,
			hostname, hostname, hostname, ch.Token, ch.Token)
	}
	return rec
}

// handleVerifyHostname runs the verifier against the active challenge
// for the URL hostname. Returns 200 with verified_at on success, 409
// with the last_error on failure, 404 if no active challenge exists.
func (c *Controller) handleVerifyHostname(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	hostname := mux.Vars(r)["hostname"]

	host, err := c.tenants.LookupActiveHostname(r.Context(), hostname)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	if host.TenantID != ac.TenantID {
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	}

	// Find the active challenge — methods are independent; we try
	// the most-recent active one across both. The caller picked the
	// method when they issued the challenge.
	var active *tenants.Challenge
	for _, m := range []string{"http-01", "dns-txt"} {
		ch, lookupErr := c.tenants.ActiveChallenge(r.Context(), host.ID, m)
		if errors.Is(lookupErr, tenants.ErrChallengeExpired) {
			writeJSONError(w, http.StatusConflict, "challenge_expired",
				map[string]any{"method": m, "hint": "issue a new challenge"})
			return
		}
		if lookupErr == nil && ch != nil {
			active = ch
			break
		}
	}
	if active == nil {
		writeJSONError(w, http.StatusNotFound, "no_active_challenge",
			map[string]any{"hint": "POST /challenges first"})
		return
	}

	v := &tenants.Verifier{
		AllowPrivateAddresses: c.pu.Conf.VerifyAllowPrivateAddresses,
	}
	var verifyErr error
	switch active.Method {
	case "dns-txt":
		verifyErr = v.VerifyDNS(r.Context(), host.Hostname, active.Token)
	case "http-01":
		verifyErr = v.VerifyHTTP(r.Context(), host.Hostname,
			hostnamePortSuffix(c.pu.Conf), active.Token)
	}

	if verifyErr != nil {
		lastErr := verifyErr.Error()
		if recordErr := c.tenants.RecordChallengeAttempt(r.Context(), active.ID, lastErr, false); recordErr != nil {
			c.pu.Logger.Warn("record challenge failure",
				zap.String("err", recordErr.Error()))
		}
		writeJSONError(w, http.StatusConflict, "verification_failed",
			map[string]any{"method": active.Method, "last_error": lastErr})
		return
	}

	now := time.Now().UTC()
	if err := c.tenants.RecordChallengeAttempt(r.Context(), active.ID, "", true); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "record_attempt",
			map[string]any{"err": err.Error()})
		return
	}

	// Build the fleet-sync artifact: existing row + verified_at stamped.
	// The challenge attempt row (tenant_hostname_challenges) is NOT
	// in the runtime-sync whitelist — those are node-local proof tokens
	// per the contract — so we only publish the hostname row update.
	postVerify := host
	postVerify.VerifiedAt = &now

	var fleetArtifactRef, fleetChecksum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadHostnameUpsert(r.Context(), postVerify)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": ferr.Error()})
			return
		}
		fleetArtifactRef, fleetChecksum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx",
			map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := c.tenants.MarkHostnameVerifiedTx(r.Context(), tx, host.ID, now); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mark_verified",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeHostnameVerified, host.TenantID, "", 0, 0,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit",
			map[string]any{"err": err.Error()})
		return
	}
	committed = true

	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after verification failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, verifyResponse{
		VerifiedAt: now.Format("2006-01-02T15:04:05Z"),
		Method:     active.Method,
	})
}

// hostnameStatusResponse is the read-only state-of-the-hostname view
// returned by GET /hostnames/{hostname}/status. Operators use it to
// see the current verify token WITHOUT mutating it (the bug the
// rotation footgun caused). The shape is intentionally a superset of
// hostnameRecord with an `active_challenges` slice so a single GET
// answers both "is this verified?" and "what token should be in DNS?".
type hostnameStatusResponse struct {
	hostnameRecord
	// ActiveChallenges lists one entry per method that has a non-
	// verified, non-revoked row in tenant_hostname_challenges (active
	// AND expired-but-still-present). Empty when the hostname has no
	// live challenge — e.g. it was already verified, or `add` ran but
	// the auto-challenge errored. Each entry carries `expired=true`
	// when the row is past its TTL so the operator sees the rotation
	// they need.
	ActiveChallenges []statusChallenge `json:"active_challenges,omitempty"`
}

type statusChallenge struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
	Expired      bool   `json:"expired,omitempty"`
	AttemptedAt  string `json:"attempted_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	Instructions string `json:"instructions"`
}

// handleHostnameStatus is the read-only counterpart to
// handleCreateChallenge: it returns the hostname row plus the current
// active challenge(s) — including the token an operator should put
// (or has put) into DNS — without rotating anything.
//
// Capability `hostname:*:read` (same as `list`): a token someone may
// place into a public DNS TXT isn't a secret in any meaningful sense
// once published; read-tier is the right gate.
//
// Was added to close the rotation footgun (internal docs/todo-custom-domains.md
// §6a): before this endpoint, the only way for an operator to "find
// out the current token" was to call `challenge`, which rotated it —
// turning every status check into a verify-fail loop.
func (c *Controller) handleHostnameStatus(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "hostname:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	hostname := mux.Vars(r)["hostname"]

	host, err := c.tenants.LookupActiveHostname(r.Context(), hostname)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_hostname",
			map[string]any{"err": err.Error()})
		return
	}
	if host.TenantID != ac.TenantID {
		writeJSONError(w, http.StatusNotFound, "hostname_not_found",
			map[string]any{"hostname": hostname})
		return
	}

	out := hostnameStatusResponse{hostnameRecord: hostnameToRecord(host)}
	for _, m := range []string{"dns-txt", "http-01"} {
		ch, lookupErr := c.tenants.ActiveChallenge(r.Context(), host.ID, m)
		expired := errors.Is(lookupErr, tenants.ErrChallengeExpired)
		if lookupErr != nil && !expired {
			// ErrNotFound is the common case (no row); only surface
			// real errors. Skip silently otherwise.
			if errors.Is(lookupErr, tenants.ErrNotFound) {
				continue
			}
			writeJSONError(w, http.StatusInternalServerError, "lookup_active_challenge",
				map[string]any{"err": lookupErr.Error(), "method": m})
			return
		}
		if ch == nil {
			continue
		}
		sc := statusChallenge{
			ID:           ch.ID,
			Method:       ch.Method,
			Token:        ch.Token,
			ExpiresAt:    ch.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
			Expired:      expired,
			LastError:    ch.LastError,
			Instructions: buildChallengeRecord(*ch, host.Hostname).Instructions,
		}
		if ch.AttemptedAt != nil {
			sc.AttemptedAt = ch.AttemptedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		out.ActiveChallenges = append(out.ActiveChallenges, sc)
	}
	writeJSON(w, http.StatusOK, out)
}

// newChallengeToken returns a random URL-safe 160-bit token with a
// stable "tcv_" prefix.
func newChallengeToken() (string, error) {
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "tcv_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}

// hostnamePortSuffix derives the URL port suffix for HTTP-01 fetches
// from the chassis's configured web inlet address. For ":8080" it
// returns ":8080"; for ":80" or "" it returns "" (default HTTP port,
// omitted). Production deployments behind an LB on port 80/443 use
// "".
func hostnamePortSuffix(conf config.Config) string {
	addr := conf.WebAddr
	if addr == "" || addr == ":80" {
		return ""
	}
	// addr is typically ":<port>" — pass through verbatim so it
	// works as a URL suffix.
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	// Host:port form — extract the port.
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if port == "80" {
			return ""
		}
		return ":" + port
	}
	return addr
}

func hostnameToRecord(h tenants.Hostname) hostnameRecord {
	rec := hostnameRecord{
		ID:        h.ID,
		Hostname:  h.Hostname,
		TenantID:  h.TenantID,
		Stack:     h.Stack,
		CreatedAt: h.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		CreatedBy: h.CreatedBy,
	}
	if h.RevokedAt != nil {
		rec.RevokedAt = h.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if h.VerifiedAt != nil {
		rec.VerifiedAt = h.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return rec
}
