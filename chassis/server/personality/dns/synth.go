package dns

import (
	"context"
	"database/sql"
	"net"
	"strings"

	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// perHostMailRRs builds the per-structured-host DKIM + DMARC TXT records for
// the default-suffix zone (origin): each chassis-minted host under the suffix
// gets its OWN `<selector>._domainkey.<host>` (public key) + `_dmarc.<host>`,
// so sending reputation is isolated per host. These are EXACT owners, so they
// win over the zone's wildcard. Best-effort: a query/scan error logs and
// yields what it has (the wildcard A/MX/SPF still serve). One filtered query
// per snapshot build — fine at tens–hundreds of hosts (see plan scale note).
func perHostMailRRs(db *sql.DB, origin string, ttl uint32, logger *zap.Logger) []dns.RR {
	rows, err := db.Query(
		`SELECT hostname, dkim_selector, dkim_public_b64 FROM tenant_hostnames
		  WHERE created_by = ? AND revoked_at IS NULL AND dkim_public_b64 != ''
		    AND hostname LIKE '%.' || ?`,
		tenants.SystemStructuredHostCreatedBy, origin)
	if err != nil {
		logger.Warn("dns: per-host mail records query failed",
			zap.String("zone", origin), zap.Error(err))
		return nil
	}
	defer rows.Close()
	var out []dns.RR
	for rows.Next() {
		var host, sel, pub string
		if err := rows.Scan(&host, &sel, &pub); err != nil {
			logger.Warn("dns: per-host mail record scan failed",
				zap.String("zone", origin), zap.Error(err))
			return out
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if sel == "" {
			sel = tenants.DKIMSelector
		}
		if rr := mkTXT(dns.Fqdn(sel+"._domainkey."+host), ttl, "v=DKIM1; k=rsa; p="+pub); rr != nil {
			out = append(out, rr)
		}
		if rr := mkTXT(dns.Fqdn("_dmarc."+host), ttl, "v=DMARC1; p=none"); rr != nil {
			out = append(out, rr)
		}
	}
	if err := rows.Err(); err != nil {
		logger.Warn("dns: per-host mail records iterate failed",
			zap.String("zone", origin), zap.Error(err))
	}
	return out
}

// SynthConfig parameterizes the synthesized record pattern for
// delegated (pattern-mode) zones. Empty fields disable the
// corresponding records — synthesis is purely additive and degrades to
// "SOA only" when nothing is configured.
type SynthConfig struct {
	Nameservers []string // FQDNs advertised as the apex NS set
	EdgeIPs     []string // A/AAAA targets for apex + per-stack hosts
	MXHost      string   // MX target hostname (the LMTP head's public name)
	MXPriority  uint16
	TTL         uint32 // TTL for synthesized records (falls back to the zone default if 0)
	// Mail-auth TXT at the apex, emitted alongside the MX (i.e. only when
	// MXHost is set). SPFOverride replaces the auto-derived SPF; DMARC is the
	// full policy string. Both overridable per-zone by a materialized
	// dns_records TXT (first-match-clears).
	SPFOverride string // "" → auto-derive from EdgeIPs + mx
	DMARC       string // e.g. "v=DMARC1; p=none"
	// StructuredSuffix is the platform's default structured-host suffix
	// (TXCO_STRUCTURED_HOST_SUFFIX), bare (no leading dot), e.g.
	// "stacks.thanks.computer". When a served zone's origin equals it, the
	// zone is the default-suffix zone and gets a WILDCARD RRset (so every
	// <stack>-<rand>.<suffix> resolves) instead of per-stack records.
	StructuredSuffix string
}

// SynthConfigFrom builds a SynthConfig from chassis config. Single
// source so the dns controller and the admin render endpoint synthesize
// identically.
func SynthConfigFrom(conf config.Config) SynthConfig {
	pri := conf.DNSMXPriority
	if pri < 0 {
		pri = 0
	}
	ttl := conf.DNSSynthTTL
	if ttl < 0 {
		ttl = 0
	}
	return SynthConfig{
		Nameservers: flattenCSV(conf.DNSNameservers),
		EdgeIPs:     flattenCSV(conf.DNSEdgeIPs),
		MXHost:      strings.TrimSpace(conf.DNSMXHost),
		MXPriority:  uint16(pri),
		TTL:         uint32(ttl),
		SPFOverride: strings.TrimSpace(conf.DNSSPF),
		DMARC:       strings.TrimSpace(conf.DNSDMARC),
		StructuredSuffix: strings.ToLower(strings.TrimSuffix(
			strings.TrimPrefix(strings.TrimSpace(conf.StructuredHostSuffix), "."), ".")),
	}
}

// flattenCSV normalizes a list flag/env value into individual entries.
// pflag splits comma-separated CLI flags into multiple elements, but
// viper's env binding does NOT — so `TXCO_DNS_NAMESERVERS=a,b` arrives
// as the single element ["a,b"]. Re-split each element on commas (and
// trim/drop blanks) so both delivery paths yield the same list.
func flattenCSV(in []string) []string {
	var out []string
	for _, e := range in {
		for _, p := range strings.Split(e, ",") {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// EffectiveSynthConfig is the synthesis config the chassis actually
// uses: the operator-set dns_settings row when present, otherwise the
// boot `--dns-*` flag defaults (flagDefaults). Read at snapshot build
// and by the admin config/zone-create endpoints — never on the query
// hot path. A read failure or a missing row falls back to the flags, so
// existing flag-only deployments are unchanged.
func EffectiveSynthConfig(db *sql.DB, flagDefaults SynthConfig) SynthConfig {
	if db == nil {
		return flagDefaults
	}
	// nil dialect ⇒ SQLite: db is the dbcache snapshot mirror (always SQLite).
	s, found, err := tenants.LoadDNSSettings(context.Background(), db, nil)
	if err != nil || !found {
		return flagDefaults
	}
	pri := s.MXPriority
	if pri < 0 {
		pri = 0
	}
	ttl := s.SynthTTL
	if ttl < 0 {
		ttl = 0
	}
	return SynthConfig{
		Nameservers: s.Nameservers,
		EdgeIPs:     s.EdgeIPs,
		MXHost:      s.MXHost,
		MXPriority:  uint16(pri),
		TTL:         uint32(ttl),
		// dns_settings carries no mail-auth/suffix columns; keep the flag
		// values so SPF/DMARC + the structured suffix stay configured even when
		// an operator sets a settings row.
		SPFOverride:      flagDefaults.SPFOverride,
		DMARC:            flagDefaults.DMARC,
		StructuredSuffix: flagDefaults.StructuredSuffix,
	}
}

// stackInfo is one active stack of a zone's tenant, used to synthesize
// per-stack records and to feed the zone serial.
type stackInfo struct {
	name        string
	activatedAt string
}

// synthesize computes the fixed record pattern for a pattern-mode zone:
// apex NS / A / AAAA (and optional MX), plus, per active non-system
// stack, `<label>.<origin>` A / AAAA + MX. The SOA is added separately
// by the caller; materialized dns_records are layered on top as
// overrides. The per-stack label is tenants.StackLabel — the SAME
// function the activation path uses to mint the routing hostname, so the
// resolved name and the routing host never diverge.
func synthesize(z *zone, cfg SynthConfig, stacks []stackInfo) []dns.RR {
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = z.defaultTTL
	}

	var out []dns.RR

	// Apex NS / A / AAAA / MX.
	for _, ns := range cfg.Nameservers {
		if rr := mkNS(z.originFQDN, ttl, ns); rr != nil {
			out = append(out, rr)
		}
	}
	out = append(out, mkAddrs(z.originFQDN, ttl, cfg.EdgeIPs)...)
	if rr := mkMX(z.originFQDN, ttl, cfg.MXPriority, cfg.MXHost); rr != nil {
		out = append(out, rr)
	}

	// Apex mail-auth TXT (SPF + DMARC), emitted alongside the MX (mail
	// enabled). SPF is softfail (~all) so it never hard-rejects a tenant's
	// other senders; DMARC defaults to p=none (monitor). Both are overridable
	// by a materialized dns_records TXT at the same owner (first-match-clears).
	if strings.TrimSpace(cfg.MXHost) != "" {
		if rr := mkTXT(z.originFQDN, ttl, effectiveSPF(cfg)); rr != nil {
			out = append(out, rr)
		}
		if rr := mkTXT(dns.Fqdn("_dmarc."+z.origin), ttl, cfg.DMARC); rr != nil {
			out = append(out, rr)
		}
	}

	// DKIM public key (per-domain, 0016). Published whenever the zone has a
	// key — the matching private key signs outbound in the sendmail op. Owner
	// is <selector>._domainkey.<origin>; the value can exceed 255 bytes, so
	// mkTXT chunks it.
	if z.dkimSelector != "" && z.dkimPubB64 != "" {
		owner := dns.Fqdn(z.dkimSelector + "._domainkey." + z.origin)
		if rr := mkTXT(owner, ttl, "v=DKIM1; k=rsa; p="+z.dkimPubB64); rr != nil {
			out = append(out, rr)
		}
	}

	// Default-suffix zone (the platform's structured-host suffix): a WILDCARD
	// RRset so every <stack>-<rand>.<suffix> resolves + can send. A/MX are
	// shared infra; SPF is the shared egress IP (per-host reputation isolation
	// is via per-host DKIM — those records are added from the structured-host
	// rows in BuildSnapshot, not here). Skip the per-stack loop: the random
	// structured labels aren't StackLabels.
	if cfg.StructuredSuffix != "" && z.origin == cfg.StructuredSuffix {
		wild := dns.Fqdn("*." + z.origin)
		out = append(out, mkAddrs(wild, ttl, cfg.EdgeIPs)...)
		if rr := mkMX(wild, ttl, cfg.MXPriority, cfg.MXHost); rr != nil {
			out = append(out, rr)
		}
		if rr := mkTXT(wild, ttl, effectiveSPF(cfg)); rr != nil {
			out = append(out, rr)
		}
		return out
	}

	// Per active stack: <label>.<origin> A/AAAA + MX.
	for _, s := range stacks {
		if !isSynthesizableStack(s.name) {
			continue
		}
		label := tenants.StackLabel(s.name)
		if label == "" {
			continue
		}
		owner := dns.Fqdn(label + "." + z.origin)
		out = append(out, mkAddrs(owner, ttl, cfg.EdgeIPs)...)
		if rr := mkMX(owner, ttl, cfg.MXPriority, cfg.MXHost); rr != nil {
			out = append(out, rr)
		}
	}
	return out
}

func mkNS(owner string, ttl uint32, ns string) dns.RR {
	ns = strings.TrimSpace(ns)
	if ns == "" {
		return nil
	}
	return &dns.NS{
		Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: ttl},
		Ns:  dns.Fqdn(ns),
	}
}

func mkMX(owner string, ttl uint32, pref uint16, host string) dns.RR {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	return &dns.MX{
		Hdr:        dns.RR_Header{Name: owner, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: ttl},
		Preference: pref,
		Mx:         dns.Fqdn(host),
	}
}

// mkTXT builds a TXT record, splitting values over 255 bytes into the
// multiple character-strings DNS requires (DKIM public keys in B2 exceed
// 255; SPF/DMARC don't, but the chunking is harmless).
func mkTXT(owner string, ttl uint32, value string) dns.RR {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &dns.TXT{
		Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl},
		Txt: chunk255(value),
	}
}

func chunk255(s string) []string {
	const max = 255
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		out = append(out, s[:max])
		s = s[max:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// effectiveSPF returns the apex SPF value: the operator override if set,
// else an auto-derived record authorizing the edge IPs + the MX host, with
// a ~all softfail so a tenant's other senders aren't hard-failed.
func effectiveSPF(cfg SynthConfig) string {
	if s := strings.TrimSpace(cfg.SPFOverride); s != "" {
		return s
	}
	var mechs []string
	for _, raw := range cfg.EdgeIPs {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			mechs = append(mechs, "ip4:"+v4.String())
		} else {
			mechs = append(mechs, "ip6:"+ip.String())
		}
	}
	if strings.TrimSpace(cfg.MXHost) != "" {
		mechs = append(mechs, "mx")
	}
	if len(mechs) == 0 {
		return ""
	}
	return "v=spf1 " + strings.Join(mechs, " ") + " ~all"
}

func mkAddrs(owner string, ttl uint32, ips []string) []dns.RR {
	var out []dns.RR
	for _, raw := range ips {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			out = append(out, &dns.A{
				Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
				A:   v4,
			})
		} else {
			out = append(out, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: owner, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl},
				AAAA: ip,
			})
		}
	}
	return out
}

// isSynthesizableStack mirrors the admin-side isMintableStack: synthesize
// records only for real, non-system stacks (skip `_`-prefixed, boot,
// txc-continuation).
func isSynthesizableStack(stack string) bool {
	if stack == "" || strings.HasPrefix(stack, "_") {
		return false
	}
	ls := strings.ToLower(stack)
	if ls == "boot" || strings.HasPrefix(ls, "boot/") || ls == "txc-continuation" {
		return false
	}
	return true
}
