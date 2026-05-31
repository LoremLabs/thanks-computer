package dns

import (
	"net"
	"strings"

	"github.com/miekg/dns"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

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
		Nameservers: conf.DNSNameservers,
		EdgeIPs:     conf.DNSEdgeIPs,
		MXHost:      conf.DNSMXHost,
		MXPriority:  uint16(pri),
		TTL:         uint32(ttl),
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
