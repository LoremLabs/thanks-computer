package admin

// Delegated-zone + override-record CRUD. A tenant registers a zone it
// has delegated to us (NS -> our chassis); the dns head then serves the
// synthesized pattern for it (chassis/server/personality/dns). Override
// records are the less-common manual layer. All mutations write
// runtime.db inside one tx and synchronously reload the dbcache mirror
// so the dns head's next snapshot rebuild sees them.
//
// Fleet note: unlike hostname CRUD, these do NOT yet emit control
// events — multi-region DNS (≥2 authoritative nodes sharing zone state)
// is a later phase; single-node uses the local Reload. See
// internal docs/todo-dns-authority.md §9.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	dnsp "github.com/loremlabs/thanks-computer/chassis/server/personality/dns"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// requireDNSZoneAccess gates the tenant-scoped DNS surface (zones,
// records, render). Default: operator-only (RequireSuperAdmin), because
// delegating zone control to tenants is a sharp edge we don't encourage.
// Escape hatch: with --dns-tenant-zone-management, a tenant holding the
// dns:* capability manages its own zones (read vs write per `write`).
// Writes the 403 itself; returns false when the caller should stop.
func (c *Controller) requireDNSZoneAccess(w http.ResponseWriter, r *http.Request, write bool) bool {
	if c.pu.Conf.DNSTenantZoneManagement {
		capName := "dns:*:read"
		if write {
			capName = "dns:*:write"
		}
		if err := policy.RequireCapability(r.Context(), capName); err != nil {
			auth.WriteForbidden(w, signature.ErrCapabilityDenied)
			return false
		}
		return true
	}
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return false
	}
	return true
}

type dnsZoneDTO struct {
	Origin     string `json:"origin"`
	Mode       string `json:"mode"`
	MName      string `json:"mname"`
	RName      string `json:"rname"`
	DefaultTTL int    `json:"default_ttl"`
	CreatedAt  string `json:"created_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"` // empty = pending (awaiting NS verification)
}

func zoneToDTO(z tenants.DNSZone) dnsZoneDTO {
	return dnsZoneDTO{
		Origin: z.Origin, Mode: z.Mode, MName: z.MName, RName: z.RName,
		DefaultTTL: z.DefaultTTL, CreatedAt: z.CreatedAt, RevokedAt: z.RevokedAt,
		VerifiedAt: z.VerifiedAt,
	}
}

type createZoneRequest struct {
	Origin string `json:"origin"`
	Mode   string `json:"mode,omitempty"` // "pattern" (default) | "manual"
}

type createZoneResponse struct {
	Zone        dnsZoneDTO `json:"zone"`
	Nameservers []string   `json:"nameservers"`
	Delegation  string     `json:"delegation"` // human-facing "set these NS records" hint
}

// handleCreateZone registers a delegated zone for the URL's tenant.
// SOA mname/rname + timers are filled from config defaults so the
// caller only supplies the origin. Requires --dns-nameservers to be
// configured (you're delegating to us; we must know our own NS names).
//
// Ownership gate (0019): with --dns-require-zone-verification off (default), the
// zone is stamped verified_at at creation and confers authority immediately —
// fine for dev / single-operator chassis where you trust the operator (and why
// creation is super-admin-only). With the flag on (multi-tenant), the zone is
// created PENDING (confers nothing) until handleVerifyZone confirms the origin's
// NS actually resolve to our nameservers — so a tenant can't squat a domain they
// don't own (`zone create stripe.com` stays inert; they can't make its NS point
// at us). The verified_at gate is enforced uniformly across the dns_zones
// authority readers (DomainCoveredByZone, TenantForMailZone,
// ActivePatternZoneOriginTx, BuildSnapshot, DKIMSignerForDomain).
func (c *Controller) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, true) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	var req createZoneRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	canon, ok := tenants.CanonicalizeHost(req.Origin)
	if !ok || !tenants.IsValidHostname(canon) {
		writeJSONError(w, http.StatusBadRequest, "invalid_origin", map[string]any{"origin": req.Origin})
		return
	}
	// Effective synthesis config = operator-set dns_settings overlaid on
	// the boot --dns-* flags. A zone can't advertise an NS set if no
	// nameservers are configured either way — refuse instead of minting
	// a zone that resolves to nothing.
	eff := dnsp.EffectiveSynthConfig(c.pu.Dbc.Snapshot(), dnsp.SynthConfigFrom(c.pu.Conf))
	nameservers := nonBlank(eff.Nameservers)
	if len(nameservers) == 0 {
		writeJSONError(w, http.StatusBadRequest, "dns_not_configured",
			map[string]any{"hint": "configure nameservers first: `txco dns config set --nameservers ns1.example.com,ns2.example.com` (or the --dns-nameservers boot flag)"})
		return
	}

	z := tenants.DNSZone{
		ID:        tenants.NewZoneID(),
		TenantID:  ac.TenantID,
		Origin:    canon,
		MName:     nameservers[0], // stored bare; synthesis Fqdn's it
		RName:     "hostmaster." + canon,
		Mode:      strings.TrimSpace(req.Mode),
		CreatedBy: ac.ActorID,
	}
	// Verification gate (0019): with --dns-require-zone-verification off (default),
	// stamp verified_at now so the zone confers authority immediately (current
	// behavior, dev / single-operator). On → leave it pending until
	// `txco dns zone verify` confirms the NS actually delegates to us.
	if !c.pu.Conf.DNSRequireZoneVerification {
		z.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
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
	if err := c.tenants.CreateZoneTx(r.Context(), tx, z); err != nil {
		switch {
		case errors.Is(err, tenants.ErrZoneExists):
			writeJSONError(w, http.StatusConflict, "zone_exists", map[string]any{"origin": canon})
		default:
			writeJSONError(w, http.StatusBadRequest, "create_zone", map[string]any{"err": err.Error()})
		}
		return
	}
	// Wire every already-active stack of this tenant into the new zone
	// (<label>.<origin>) and fleet-publish the routing rows. Without this a
	// zone created AFTER its stacks were activated leaves them unrouted (the
	// activation-time mint only fires when the zone already exists), and on a
	// multi-node fleet only the admin node would hold the rows. Pattern mode
	// only — manual zones synthesize nothing. See dns_fleet.go.
	// A pending zone (verification on) confers nothing yet, so defer wiring
	// stacks + fleet-publishing to verify time (handleVerifyZone). Verified at
	// creation (default) → do them now.
	if z.VerifiedAt != "" && (z.Mode == "" || strings.EqualFold(z.Mode, "pattern")) {
		if err := c.reconcileZoneHostnames(r.Context(), tx, z.TenantID, z.Origin); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "reconcile_zone_hostnames",
				map[string]any{"err": err.Error()})
			return
		}
	}
	// Fleet-sync the zone row so data-plane nodes hold the delegated-zone state
	// (re-derive routing hosts on future activations; serve it with the dns
	// head). Read the persisted row so CreateZoneTx's SOA + timestamp defaults
	// ride along.
	if z.VerifiedAt != "" && c.fleetEnabled() {
		persisted, gerr := tenants.GetZoneByIDTx(r.Context(), tx, z.ID)
		if gerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "load_zone", map[string]any{"err": gerr.Error()})
			return
		}
		if err := c.fleetPublishZone(r.Context(), tx, persisted); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "publish_zone", map[string]any{"err": err.Error()})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns zone create failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	zoneSOADefaultsForDTO(&z)
	delegation := delegationHint(canon, nameservers)
	if z.VerifiedAt == "" {
		delegation += "\n\nThe zone is PENDING — it serves no records, routes no mail, and" +
			" signs no DKIM until verified. Once the NS records above resolve, run:\n  txco dns zone verify " + canon
	}
	writeJSON(w, http.StatusCreated, createZoneResponse{
		Zone:        zoneToDTO(z),
		Nameservers: nameservers,
		Delegation:  delegation,
	})
}

// handleVerifyZone confirms a (pending) zone's NS actually delegates to our
// nameservers, then flips it live — wiring already-active stacks into it and
// fleet-publishing the now-verified row. This is the anti-squat gate: a tenant
// can't make a domain they don't own delegate to us, so they can't verify (and
// thus can't gain DKIM/verified-sender/routing authority over) it. Idempotent
// for an already-verified zone.
func (c *Controller) handleVerifyZone(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, true) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	rawOrigin := mux.Vars(r)["origin"]
	origin, ok := tenants.CanonicalizeHost(rawOrigin)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_origin", map[string]any{"origin": rawOrigin})
		return
	}
	zone, err := c.tenants.LookupActiveZone(r.Context(), ac.TenantID, origin)
	switch {
	case errors.Is(err, tenants.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "zone_not_found", map[string]any{"origin": origin})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_zone", map[string]any{"err": err.Error()})
		return
	}

	eff := dnsp.EffectiveSynthConfig(c.pu.Dbc.Snapshot(), dnsp.SynthConfigFrom(c.pu.Conf))
	nameservers := nonBlank(eff.Nameservers)
	if len(nameservers) == 0 {
		writeJSONError(w, http.StatusBadRequest, "dns_not_configured",
			map[string]any{"hint": "no nameservers configured to verify against"})
		return
	}

	// The check: does origin's NS (resolved against public DNS) point at us?
	verified, resolved, nerr := tenants.ZoneNSVerified(r.Context(), origin, nameservers)
	if nerr != nil {
		writeJSONError(w, http.StatusBadGateway, "ns_lookup_failed",
			map[string]any{"origin": origin, "err": nerr.Error(),
				"hint": "the NS records may not have propagated yet — retry shortly"})
		return
	}
	if !verified {
		writeJSONError(w, http.StatusConflict, "ns_not_delegated",
			map[string]any{"origin": origin, "want": nameservers, "resolved": resolved,
				"hint": "set the NS records from `txco dns zone create` at your registrar, then retry once they propagate"})
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
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
	if err := tenants.SetZoneVerifiedTx(r.Context(), tx, ac.TenantID, origin, now); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "set_verified", map[string]any{"err": err.Error()})
		return
	}
	// Now live: wire already-active stacks into the zone + fleet-publish the
	// verified row (mirrors the verified-at-create path in handleCreateZone).
	if zone.Mode == "" || strings.EqualFold(zone.Mode, "pattern") {
		if err := c.reconcileZoneHostnames(r.Context(), tx, ac.TenantID, origin); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "reconcile_zone_hostnames", map[string]any{"err": err.Error()})
			return
		}
	}
	if c.fleetEnabled() {
		persisted, gerr := tenants.GetZoneByIDTx(r.Context(), tx, zone.ID)
		if gerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "load_zone", map[string]any{"err": gerr.Error()})
			return
		}
		if err := c.fleetPublishZone(r.Context(), tx, persisted); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "publish_zone", map[string]any{"err": err.Error()})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns zone verify failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"origin": origin, "verified_at": now})
}

// handleListZones lists the tenant's zones (?history=true includes revoked).
func (c *Controller) handleListZones(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, false) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	zones, err := c.tenants.ListZones(r.Context(), ac.TenantID, r.URL.Query().Get("history") == "true")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_zones", map[string]any{"err": err.Error()})
		return
	}
	out := make([]dnsZoneDTO, 0, len(zones))
	for _, z := range zones {
		out = append(out, zoneToDTO(z))
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": out})
}

// handleRevokeZone soft-revokes a tenant's zone by origin (idempotent).
func (c *Controller) handleRevokeZone(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, true) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	origin := mux.Vars(r)["origin"]

	// Capture the zone id before revoking so the revoke can be fleet-published
	// (the row-upsert is keyed by id). No active zone ⇒ RevokeZoneTx is a no-op
	// and there's nothing to propagate.
	var zoneID string
	if z, lerr := c.tenants.LookupActiveZone(r.Context(), ac.TenantID, origin); lerr == nil {
		zoneID = z.ID
	} else if !errors.Is(lerr, tenants.ErrNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "lookup_zone", map[string]any{"err": lerr.Error()})
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
	canon, rerr := c.tenants.RevokeZoneTx(r.Context(), tx, ac.TenantID, origin)
	if rerr != nil && !errors.Is(rerr, tenants.ErrNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "revoke_zone", map[string]any{"err": rerr.Error()})
		return
	}
	// Propagate the now-revoked row (revoked_at set) so data-plane nodes drop
	// the zone from their state too.
	if c.fleetEnabled() && zoneID != "" {
		persisted, gerr := tenants.GetZoneByIDTx(r.Context(), tx, zoneID)
		if gerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "load_zone", map[string]any{"err": gerr.Error()})
			return
		}
		if err := c.fleetPublishZone(r.Context(), tx, persisted); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "publish_zone", map[string]any{"err": err.Error()})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns zone revoke failed; FS watcher will retry", zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "origin": canon})
}

type dnsRecordDTO struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   *int64 `json:"ttl,omitempty"`
	Rdata string `json:"rdata"`
}

type createRecordRequest struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   *int64 `json:"ttl,omitempty"`
	Rdata string `json:"rdata"`
}

// handleCreateRecord adds an override/extra record under a tenant zone.
func (c *Controller) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, true) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	zone, ok := c.lookupTenantZone(w, r, ac.TenantID)
	if !ok {
		return
	}

	var req createRecordRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	if !tenants.ValidDNSRecordType(req.Type) {
		writeJSONError(w, http.StatusBadRequest, "invalid_type", map[string]any{"type": req.Type})
		return
	}
	rec := tenants.DNSRecord{
		ID:        tenants.NewRecordID(),
		ZoneID:    zone.ID,
		Name:      req.Name,
		Type:      req.Type,
		Rdata:     req.Rdata,
		CreatedBy: ac.ActorID,
	}
	if req.TTL != nil {
		rec.TTL = sql.NullInt64{Int64: *req.TTL, Valid: true}
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
	if err := c.tenants.CreateRecordTx(r.Context(), tx, rec); err != nil {
		writeJSONError(w, http.StatusBadRequest, "create_record", map[string]any{"err": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns record create failed; FS watcher will retry", zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusCreated, recordToDTO(rec))
}

// handleListRecords lists active override records for a tenant zone.
func (c *Controller) handleListRecords(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, false) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	zone, ok := c.lookupTenantZone(w, r, ac.TenantID)
	if !ok {
		return
	}
	recs, err := c.tenants.ListRecords(r.Context(), zone.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_records", map[string]any{"err": err.Error()})
		return
	}
	out := make([]dnsRecordDTO, 0, len(recs))
	for _, rec := range recs {
		out = append(out, recordToDTO(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}

// handleRevokeRecord soft-revokes records matching (?name, ?type) under
// a tenant zone.
func (c *Controller) handleRevokeRecord(w http.ResponseWriter, r *http.Request) {
	if !c.requireDNSZoneAccess(w, r, true) {
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	zone, ok := c.lookupTenantZone(w, r, ac.TenantID)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	rtype := r.URL.Query().Get("type")
	if !tenants.ValidDNSRecordType(rtype) {
		writeJSONError(w, http.StatusBadRequest, "invalid_type", map[string]any{"type": rtype})
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
	rerr := c.tenants.RevokeRecordTx(r.Context(), tx, zone.ID, name, rtype)
	if rerr != nil && !errors.Is(rerr, tenants.ErrNotFound) {
		writeJSONError(w, http.StatusInternalServerError, "revoke_record", map[string]any{"err": rerr.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns record revoke failed; FS watcher will retry", zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "name": name, "type": strings.ToUpper(rtype)})
}

// lookupTenantZone resolves the {origin} path var to one of the
// caller's active zones, writing a 404 and returning ok=false on miss
// (no cross-tenant peek).
func (c *Controller) lookupTenantZone(w http.ResponseWriter, r *http.Request, tenantID string) (tenants.DNSZone, bool) {
	origin := mux.Vars(r)["origin"]
	z, err := c.tenants.LookupActiveZone(r.Context(), tenantID, origin)
	if errors.Is(err, tenants.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "zone_not_found", map[string]any{"origin": origin})
		return tenants.DNSZone{}, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_zone", map[string]any{"err": err.Error()})
		return tenants.DNSZone{}, false
	}
	return z, true
}

func recordToDTO(r tenants.DNSRecord) dnsRecordDTO {
	d := dnsRecordDTO{Name: r.Name, Type: strings.ToUpper(r.Type), Rdata: r.Rdata}
	if r.TTL.Valid {
		v := r.TTL.Int64
		d.TTL = &v
	}
	return d
}

// nonBlank drops empty/whitespace entries from a config []string.
func nonBlank(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// delegationHint renders the customer-facing "point your NS at us" line.
func delegationHint(origin string, nameservers []string) string {
	var b strings.Builder
	b.WriteString("Delegate ")
	b.WriteString(origin)
	b.WriteString(" by setting NS records at your registrar:")
	for _, ns := range nameservers {
		b.WriteString("\n  ")
		b.WriteString(origin)
		b.WriteString(". NS ")
		b.WriteString(withTrailingDot(ns))
	}
	return b.String()
}

func withTrailingDot(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

// zoneSOADefaultsForDTO mirrors the service-layer defaults so the
// create response reflects what was actually stored.
func zoneSOADefaultsForDTO(z *tenants.DNSZone) {
	if z.Mode == "" {
		z.Mode = "pattern"
	}
	if z.DefaultTTL == 0 {
		z.DefaultTTL = 300
	}
}
