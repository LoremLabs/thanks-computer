package admin

// Chassis-global DNS synthesis config (`txco dns config show|set`). The
// nameservers / edge IPs / MX host that parameterize the synthesized
// pattern are deployment infrastructure, not per-tenant data, so these
// endpoints are NOT under /v1/tenants/{t}. Writes upsert the singleton
// dns_settings row and reload the dbcache mirror so the dns head picks
// the new values up with no restart. Boot `--dns-*` flags remain the
// fallback/seed when no row exists. See internal docs/todo-dns-authority.md.

import (
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	dnsp "github.com/loremlabs/thanks-computer/chassis/server/personality/dns"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

type dnsConfigDTO struct {
	Nameservers []string `json:"nameservers"`
	EdgeIPs     []string `json:"edge_ips"`
	MXHost      string   `json:"mx_host"`
	MXPriority  int      `json:"mx_priority"`
	TTL         int      `json:"ttl"`
	// Configured is true when an operator-set dns_settings row exists;
	// false means the values shown are the boot --dns-* flag defaults.
	Configured bool `json:"configured"`
}

type putDNSConfigRequest struct {
	Nameservers *[]string `json:"nameservers,omitempty"`
	EdgeIPs     *[]string `json:"edge_ips,omitempty"`
	MXHost      *string   `json:"mx_host,omitempty"`
	MXPriority  *int      `json:"mx_priority,omitempty"`
	TTL         *int      `json:"ttl,omitempty"`
}

// effectiveDNSConfigDTO renders the config the dns head will actually
// use (settings row overlaid on flags) plus whether a row exists.
func (c *Controller) effectiveDNSConfigDTO(ctx context.Context) dnsConfigDTO {
	eff := dnsp.EffectiveSynthConfig(c.pu.Dbc.Snapshot(), dnsp.SynthConfigFrom(c.pu.Conf))
	_, configured, _ := tenants.LoadDNSSettings(ctx, c.pu.RuntimeDB, c.pu.RuntimeDialect)
	return dnsConfigDTO{
		Nameservers: eff.Nameservers,
		EdgeIPs:     eff.EdgeIPs,
		MXHost:      eff.MXHost,
		MXPriority:  int(eff.MXPriority),
		TTL:         int(eff.TTL),
		Configured:  configured,
	}
}

// handleGetDNSConfig returns the effective chassis synthesis config.
//
// Operator-only (RequireSuperAdmin) — this is chassis-wide deployment
// infrastructure shared by every tenant's zones, NOT per-tenant data.
// A tenant gets the nameservers it needs to delegate to from its own
// `dns zone create` response, so it never needs this global view.
func (c *Controller) handleGetDNSConfig(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	writeJSON(w, http.StatusOK, c.effectiveDNSConfigDTO(r.Context()))
}

// handlePutDNSConfig sets (read-modify-write) the chassis synthesis
// config. Only the provided fields change; the rest are preserved from
// the current effective config. Reloads the mirror so synthesis picks
// the change up with no restart.
func (c *Controller) handlePutDNSConfig(w http.ResponseWriter, r *http.Request) {
	// Operator-only: this sets chassis-wide infrastructure that affects
	// EVERY tenant's synthesized zones. RequireSuperAdmin (the same gate
	// as tenant creation), NOT the per-tenant `dns:*:write` a tenant
	// uses to manage its own zones.
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())

	var req putDNSConfigRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}

	// Start from the current PERSISTED config so a partial set preserves the
	// other fields (first set seeds from the boot flags). Read the settings row
	// from the authoritative runtime DB, NOT the dbcache mirror: a partial PUT
	// landing while the mirror is stale would otherwise clobber the unspecified
	// fields with stale values. PutDNSSettingsTx always writes all columns
	// (clamped), so a found row is complete and needs no flag overlay.
	flags := dnsp.SynthConfigFrom(c.pu.Conf)
	s := tenants.DNSSettings{
		Nameservers: flags.Nameservers,
		EdgeIPs:     flags.EdgeIPs,
		MXHost:      flags.MXHost,
		MXPriority:  int(flags.MXPriority),
		SynthTTL:    int(flags.TTL),
	}
	// Distinguish "no row yet" (seed from flags) from a read failure: a
	// transient error must NOT fall through to boot defaults, or a partial
	// PUT would clobber all unspecified persisted fields. Fail the request.
	row, found, lerr := tenants.LoadDNSSettings(r.Context(), c.pu.RuntimeDB, c.pu.RuntimeDialect)
	if lerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_dns_config", map[string]any{"err": lerr.Error()})
		return
	}
	if found {
		s.Nameservers = row.Nameservers
		s.EdgeIPs = row.EdgeIPs
		s.MXHost = row.MXHost
		s.MXPriority = row.MXPriority
		s.SynthTTL = row.SynthTTL
	}
	if ac != nil {
		s.UpdatedBy = ac.ActorID
	}
	if req.Nameservers != nil {
		s.Nameservers = *req.Nameservers
	}
	if req.EdgeIPs != nil {
		s.EdgeIPs = *req.EdgeIPs
	}
	if req.MXHost != nil {
		s.MXHost = *req.MXHost
	}
	if req.MXPriority != nil {
		s.MXPriority = *req.MXPriority
	}
	if req.TTL != nil {
		s.SynthTTL = *req.TTL
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
	persisted, perr := tenants.PutDNSSettingsTx(r.Context(), tx, s, c.pu.RuntimeDialect)
	if perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "put_dns_config", map[string]any{"err": perr.Error()})
		return
	}
	// Fleet-sync the singleton so every dns head synthesizes from the same
	// config. Without this event the set was admin-local: heads kept their
	// boot-flag view until an unrelated event nudged a reload (the same
	// silent gap dns.record.upserted closed).
	if err := c.fleetPublishDNSSettings(r.Context(), tx, persisted); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "publish_dns_config", map[string]any{"err": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	// Deliberately SYNCHRONOUS on every runtime (not ReloadAfterWrite):
	// the response below is effectiveDNSConfigDTO, which reads the just-
	// written settings back through the MIRROR (EffectiveSynthConfig over
	// Dbc.Snapshot()) — with a background reload it would echo the
	// pre-write config. The one write handler whose response depends on
	// the reload; see todo-control-plane-reload-scaling.md §③.
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after dns config set failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, c.effectiveDNSConfigDTO(r.Context()))
}
