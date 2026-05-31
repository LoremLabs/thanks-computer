// Package dns implements the chassis's authoritative-DNS head: a
// personality that answers DNS queries for zones explicitly delegated
// to this chassis, straight from an in-memory snapshot of the
// dns_zones/dns_records tables.
//
// Phase 1 scope (internal docs/todo-dns-authority.md): materialized
// records only — no record synthesis, no DNS-01, no DNSSEC. The server
// is authoritative-only and NEVER recursive: a query whose name falls
// under no served zone is REFUSED.
//
// Data-plane discipline: a query runs NO opstack and never touches the
// bus. It is answered from a prebuilt ZoneSnapshot that is rebuilt on
// config-apply (dbcache OnReload), so the hot path does zero DB reads —
// the same "no syscalls on the hot path" rule the static-asset index
// and redaction registry follow.
package dns

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// zone is one delegated zone's prebuilt answer set. Everything here is
// computed once at snapshot build and never mutated, so reads are
// lock-free.
type zone struct {
	tenantID   string
	origin     string // canonical apex, lowercased, NO trailing dot ("ops.example.com")
	originFQDN string // dns.Fqdn(origin), lowercased ("ops.example.com.")
	soa        *dns.SOA
	defaultTTL uint32
	serial     uint32

	// rr indexes answers by lowercased owner FQDN → qtype → RRs. The
	// synthesized SOA is included under TypeSOA at the apex so SOA
	// queries answer from the snapshot like any other type.
	rr map[string]map[uint16][]dns.RR

	// names is the set of lowercased owner FQDNs that exist in the zone
	// (apex always included). Drives the NODATA (name exists, type
	// absent) vs NXDOMAIN (name absent) distinction.
	names map[string]bool
}

// ZoneSnapshot is an immutable, prebuilt view of every served zone.
// Build it with BuildSnapshot; serve from it with Lookup; preview it
// with Render. Swap a whole *ZoneSnapshot atomically on reload — never
// mutate one in place.
type ZoneSnapshot struct {
	// zones sorted by originFQDN length descending, so the first
	// suffix match in zoneFor is the most specific (longest) zone.
	zones []*zone
}

// BuildSnapshot reads all active zones + records from the runtime
// mirror and assembles a ZoneSnapshot. A malformed individual record is
// logged and skipped (best-effort, like the LMTP MIME parse) rather
// than darkening the whole zone; only a DB-level failure returns an
// error. Pass dbc.Snapshot() — never a captured dbc.Db handle.
func BuildSnapshot(db *sql.DB, logger *zap.Logger) (*ZoneSnapshot, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	zrows, err := db.Query(`SELECT id, tenant_id, origin, mname, rname,
	                               refresh, retry, expire, minimum,
	                               default_ttl, updated_at
	                          FROM dns_zones
	                         WHERE revoked_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("dns: query zones: %w", err)
	}
	type zoneRow struct {
		id, tenantID, origin, mname, rname string
		refresh, retry, expire, minimum    uint32
		defaultTTL                         uint32
		updatedAt                          string
	}
	var zoneRows []zoneRow
	for zrows.Next() {
		var z zoneRow
		if err := zrows.Scan(&z.id, &z.tenantID, &z.origin, &z.mname, &z.rname,
			&z.refresh, &z.retry, &z.expire, &z.minimum, &z.defaultTTL, &z.updatedAt); err != nil {
			zrows.Close()
			return nil, fmt.Errorf("dns: scan zone: %w", err)
		}
		zoneRows = append(zoneRows, z)
	}
	if err := zrows.Err(); err != nil {
		zrows.Close()
		return nil, fmt.Errorf("dns: iterate zones: %w", err)
	}
	zrows.Close()

	snap := &ZoneSnapshot{}
	for _, zr := range zoneRows {
		origin := strings.ToLower(strings.TrimSuffix(zr.origin, "."))
		z := &zone{
			tenantID:   zr.tenantID,
			origin:     origin,
			originFQDN: dns.Fqdn(origin),
			defaultTTL: zr.defaultTTL,
			rr:         map[string]map[uint16][]dns.RR{},
			names:      map[string]bool{},
		}
		// The apex always exists (it carries SOA + NS).
		z.names[z.originFQDN] = true

		// max(updated_at) over the zone row + its records drives the
		// serial. Start from the zone's own updated_at.
		maxT, _ := parseTS(zr.updatedAt)

		rrows, rerr := db.Query(`SELECT name, type, ttl, rdata, updated_at
		                           FROM dns_records
		                          WHERE zone_id = ? AND revoked_at IS NULL`, zr.id)
		if rerr != nil {
			return nil, fmt.Errorf("dns: query records for %s: %w", origin, rerr)
		}
		for rrows.Next() {
			var name, rtype, rdata, updatedAt string
			var ttl sql.NullInt64
			if err := rrows.Scan(&name, &rtype, &ttl, &rdata, &updatedAt); err != nil {
				rrows.Close()
				return nil, fmt.Errorf("dns: scan record for %s: %w", origin, err)
			}
			if t, ok := parseTS(updatedAt); ok && t.After(maxT) {
				maxT = t
			}
			effTTL := z.defaultTTL
			if ttl.Valid && ttl.Int64 >= 0 {
				effTTL = uint32(ttl.Int64)
			}
			rr, perr := buildRR(z, name, rtype, effTTL, rdata)
			if perr != nil {
				logger.Warn("dns: skipping malformed record",
					zap.String("origin", origin),
					zap.String("name", name),
					zap.String("type", rtype),
					zap.String("rdata", rdata),
					zap.String("err", perr.Error()))
				continue
			}
			z.add(rr)
		}
		if err := rrows.Err(); err != nil {
			rrows.Close()
			return nil, fmt.Errorf("dns: iterate records for %s: %w", origin, err)
		}
		rrows.Close()

		// Serial = uint32 epoch-seconds (UTC) of the latest change to
		// this zone's content; clamp to >=1 (RFC 1912 advises serial!=0).
		// Per-zone + content-derived: a no-op reload never advances it.
		serial := uint32(maxT.UTC().Unix())
		if serial == 0 {
			serial = 1
		}
		z.serial = serial

		// Synthesize the SOA from the zone columns + computed serial and
		// index it at the apex so SOA queries answer from the snapshot.
		z.soa = &dns.SOA{
			Hdr:     dns.RR_Header{Name: z.originFQDN, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: z.defaultTTL},
			Ns:      dns.Fqdn(zr.mname),
			Mbox:    dns.Fqdn(zr.rname),
			Serial:  serial,
			Refresh: zr.refresh,
			Retry:   zr.retry,
			Expire:  zr.expire,
			Minttl:  zr.minimum,
		}
		z.add(z.soa)

		snap.zones = append(snap.zones, z)
	}

	// Longest origin first → zoneFor's first suffix match is the most
	// specific zone (matters once nested delegations exist).
	sort.Slice(snap.zones, func(i, j int) bool {
		return len(snap.zones[i].originFQDN) > len(snap.zones[j].originFQDN)
	})
	return snap, nil
}

// add inserts an already-built RR into the zone's index + name set.
func (z *zone) add(rr dns.RR) {
	owner := strings.ToLower(rr.Header().Name)
	byType := z.rr[owner]
	if byType == nil {
		byType = map[uint16][]dns.RR{}
		z.rr[owner] = byType
	}
	byType[rr.Header().Rrtype] = append(byType[rr.Header().Rrtype], rr)
	z.names[owner] = true
}

// buildRR turns a stored record into a dns.RR by composing a
// presentation-format line and parsing it with dns.NewRR. rdata is the
// RDATA portion exactly as it appears in a zone file; TXT is forgiving
// (a bare unquoted value is wrapped automatically).
func buildRR(z *zone, name, rtype string, ttl uint32, rdata string) (dns.RR, error) {
	var ownerFQDN string
	if name == "@" || name == "" {
		ownerFQDN = z.originFQDN
	} else {
		ownerFQDN = dns.Fqdn(strings.ToLower(name) + "." + z.origin)
	}
	rdataPres := rdata
	if strings.EqualFold(rtype, "TXT") {
		rdataPres = txtRdata(rdata)
	}
	line := fmt.Sprintf("%s %d IN %s %s", ownerFQDN, ttl, strings.ToUpper(rtype), rdataPres)
	rr, err := dns.NewRR(line)
	if err != nil {
		return nil, err
	}
	if rr == nil {
		return nil, fmt.Errorf("empty RR from %q", line)
	}
	return rr, nil
}

// txtRdata returns a quoted TXT presentation value. A value already
// starting with a quote is assumed pre-formatted (possibly multiple
// character-strings) and passed through; otherwise the whole string is
// wrapped as a single quoted character-string with the two presentation
// metacharacters escaped.
func txtRdata(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "\"") {
		return raw
	}
	esc := strings.ReplaceAll(raw, "\\", "\\\\")
	esc = strings.ReplaceAll(esc, "\"", "\\\"")
	return "\"" + esc + "\""
}

// Lookup resolves a single question against the snapshot.
//
//	rcode REFUSED               → no served zone covers the name (we are
//	                              authoritative-only, never recursive)
//	rcode NOERROR + answer      → matching RRset
//	rcode NOERROR + ns(SOA)     → NODATA: name exists, type absent
//	rcode NXDOMAIN + ns(SOA)    → name does not exist in the zone
//
// ANY is refused (no ANY expansion — anti-amplification). CNAME chasing
// and wildcards are out of scope for Phase 1.
func (s *ZoneSnapshot) Lookup(q dns.Question) (answer, ns []dns.RR, rcode int) {
	qname := strings.ToLower(dns.Fqdn(q.Name))
	z := s.zoneFor(qname)
	if z == nil {
		return nil, nil, dns.RcodeRefused
	}
	if q.Qtype == dns.TypeANY {
		// RFC 8482: refuse to expand ANY rather than amplify.
		return nil, nil, dns.RcodeRefused
	}
	if byType, ok := z.rr[qname]; ok {
		if rrs := byType[q.Qtype]; len(rrs) > 0 {
			return rrs, nil, dns.RcodeSuccess
		}
		// Name exists, requested type doesn't → NODATA.
		return nil, []dns.RR{z.soa}, dns.RcodeSuccess
	}
	if z.names[qname] {
		return nil, []dns.RR{z.soa}, dns.RcodeSuccess
	}
	return nil, []dns.RR{z.soa}, dns.RcodeNameError
}

// zoneFor returns the most specific served zone whose origin is a suffix
// of qname (which must already be a lowercased FQDN), or nil.
func (s *ZoneSnapshot) zoneFor(qname string) *zone {
	for _, z := range s.zones {
		if qname == z.originFQDN || strings.HasSuffix(qname, "."+z.originFQDN) {
			return z
		}
	}
	return nil
}

// byOrigin returns the served zone for a canonical origin (case- and
// trailing-dot-insensitive), or nil.
func (s *ZoneSnapshot) byOrigin(origin string) *zone {
	want := strings.ToLower(strings.TrimSuffix(origin, "."))
	for _, z := range s.zones {
		if z.origin == want {
			return z
		}
	}
	return nil
}

// OriginsForTenant returns the canonical origins served for a tenant,
// sorted. Used by the admin render endpoint.
func (s *ZoneSnapshot) OriginsForTenant(tenantID string) []string {
	var out []string
	for _, z := range s.zones {
		if z.tenantID == tenantID {
			out = append(out, z.origin)
		}
	}
	sort.Strings(out)
	return out
}

// Render emits the zone TxCo would serve for origin in standard
// zone-file (presentation) form, or ok=false if the origin isn't
// served. The header comment carries the UTC generation stamp (the
// serial formatted as an RFC3339 instant) so an operator reads one
// unambiguous value; the SOA serial is the same number on the wire.
func (s *ZoneSnapshot) Render(origin string) (string, bool) {
	z := s.byOrigin(origin)
	if z == nil {
		return "", false
	}
	var b strings.Builder
	genUTC := time.Unix(int64(z.serial), 0).UTC().Format(time.RFC3339)
	fmt.Fprintf(&b, "; zone %s — generation %s (serial %d)\n", z.origin, genUTC, z.serial)
	b.WriteString(z.soa.String() + "\n")

	var rrs []dns.RR
	for _, byType := range z.rr {
		for t, list := range byType {
			if t == dns.TypeSOA {
				continue // already emitted above
			}
			rrs = append(rrs, list...)
		}
	}
	sort.Slice(rrs, func(i, j int) bool {
		hi, hj := rrs[i].Header(), rrs[j].Header()
		if hi.Name != hj.Name {
			return hi.Name < hj.Name
		}
		if hi.Rrtype != hj.Rrtype {
			return hi.Rrtype < hj.Rrtype
		}
		return rrs[i].String() < rrs[j].String()
	})
	for _, rr := range rrs {
		b.WriteString(rr.String() + "\n")
	}
	return b.String(), true
}

// parseTS parses a stored RFC3339(-ish) timestamp into UTC. Tries the
// nano and millis variants the chassis writes (applier uses a
// millisecond layout; tenant tables use RFC3339).
func parseTS(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
