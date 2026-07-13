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

	dkimSelector string // DKIM selector for the zone's published key (0016)
	dkimPubB64   string // base64 PKIX DER public key → <selector>._domainkey TXT

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
func BuildSnapshot(db *sql.DB, cfg SynthConfig, logger *zap.Logger) (*ZoneSnapshot, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	zrows, err := db.Query(`SELECT id, tenant_id, origin, mname, rname,
	                               refresh, retry, expire, minimum,
	                               default_ttl, mode, updated_at,
	                               dkim_selector, dkim_public_b64
	                          FROM dns_zones
	                         WHERE revoked_at IS NULL AND verified_at IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("dns: query zones: %w", err)
	}
	type zoneRow struct {
		id, tenantID, origin, mname, rname string
		refresh, retry, expire, minimum    uint32
		defaultTTL                         uint32
		mode, updatedAt                    string
		dkimSelector, dkimPubB64           string
	}
	var zoneRows []zoneRow
	for zrows.Next() {
		var z zoneRow
		if err := zrows.Scan(&z.id, &z.tenantID, &z.origin, &z.mname, &z.rname,
			&z.refresh, &z.retry, &z.expire, &z.minimum, &z.defaultTTL, &z.mode, &z.updatedAt,
			&z.dkimSelector, &z.dkimPubB64); err != nil {
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

	// Active stacks per tenant, loaded once (fully read + closed before
	// the per-zone record queries — required under the mirror's single
	// pinned connection). Feeds per-stack synthesis + the serial.
	stacksByTenant, serr := loadActiveStacks(db)
	if serr != nil {
		return nil, serr
	}

	// Effective synthesis config: the operator-set dns_settings row if
	// present, else the boot-flag defaults passed in `cfg`. (Per-zone
	// overrides will overlay this per zone in a later phase.)
	eff := EffectiveSynthConfig(db, cfg)

	snap := &ZoneSnapshot{}
	for _, zr := range zoneRows {
		origin := strings.ToLower(strings.TrimSuffix(zr.origin, "."))
		z := &zone{
			tenantID:     zr.tenantID,
			origin:       origin,
			originFQDN:   dns.Fqdn(origin),
			defaultTTL:   zr.defaultTTL,
			dkimSelector: zr.dkimSelector,
			dkimPubB64:   zr.dkimPubB64,
			rr:           map[string]map[uint16][]dns.RR{},
			names:        map[string]bool{},
		}
		// The apex always exists (it carries SOA + NS).
		z.names[z.originFQDN] = true

		// max(updated_at) over the zone row + its records (+ active-stack
		// activations, for pattern zones) drives the serial.
		maxT, _ := parseTS(zr.updatedAt)

		// Read materialized records fully into a slice, then close — so
		// no record cursor is open during synthesis or the next zone.
		type recRow struct {
			name, rtype, rdata, updatedAt string
			ttl                           sql.NullInt64
		}
		var matRecs []recRow
		rrows, rerr := db.Query(`SELECT name, type, ttl, rdata, updated_at
		                           FROM dns_records
		                          WHERE zone_id = ? AND revoked_at IS NULL`, zr.id)
		if rerr != nil {
			return nil, fmt.Errorf("dns: query records for %s: %w", origin, rerr)
		}
		for rrows.Next() {
			var rec recRow
			if err := rrows.Scan(&rec.name, &rec.rtype, &rec.ttl, &rec.rdata, &rec.updatedAt); err != nil {
				rrows.Close()
				return nil, fmt.Errorf("dns: scan record for %s: %w", origin, err)
			}
			if t, ok := parseTS(rec.updatedAt); ok && t.After(maxT) {
				maxT = t
			}
			matRecs = append(matRecs, rec)
		}
		if err := rrows.Err(); err != nil {
			rrows.Close()
			return nil, fmt.Errorf("dns: iterate records for %s: %w", origin, err)
		}
		rrows.Close()

		// 'pattern' (default/empty) synthesizes the fixed shape, then lets
		// materialized records override; 'manual' is materialized-only.
		pattern := zr.mode != "manual"
		if pattern {
			stacks := stacksByTenant[zr.tenantID]
			for _, s := range stacks {
				if t, ok := parseTS(s.activatedAt); ok && t.After(maxT) {
					maxT = t
				}
			}
			for _, rr := range synthesize(z, eff, stacks) {
				z.add(rr)
			}
			// Default-suffix zone: per-structured-host DKIM/DMARC TXT
			// (reputation isolation — each host signs d=<host> with its own
			// key). Exact owners, so they win over the wildcard from
			// synthesize(). One filtered query per reload (see scale note).
			if eff.StructuredSuffix != "" && z.origin == eff.StructuredSuffix {
				stTTL := eff.TTL
				if stTTL == 0 {
					stTTL = z.defaultTTL
				}
				for _, rr := range perHostMailRRs(db, z.origin, stTTL, logger) {
					z.add(rr)
				}
			}
		}

		// Materialized records: in pattern mode the FIRST record for a
		// given (owner,type) clears the synthesized set for that
		// (owner,type) (override); subsequent ones of the same key add
		// to it. A CNAME clears EVERY synthesized type at its owner —
		// a CNAME owner carries no other data (RFC 1034 §3.6.2), so a
		// synthesized A left beside it would serve an illegal node.
		// In manual mode there is nothing synthesized to clear.
		cleared := map[string]bool{}
		for _, rec := range matRecs {
			effTTL := z.defaultTTL
			if rec.ttl.Valid && rec.ttl.Int64 >= 0 {
				effTTL = uint32(rec.ttl.Int64)
			}
			rr, perr := buildRR(z, rec.name, rec.rtype, effTTL, rec.rdata)
			if perr != nil {
				logger.Warn("dns: skipping malformed record",
					zap.String("origin", origin),
					zap.String("name", rec.name),
					zap.String("type", rec.rtype),
					zap.String("rdata", rec.rdata),
					zap.String("err", perr.Error()))
				continue
			}
			owner := strings.ToLower(rr.Header().Name)
			rtype := rr.Header().Rrtype
			if pattern {
				key := fmt.Sprintf("%s|%d", owner, rtype)
				if !cleared[key] {
					if rtype == dns.TypeCNAME {
						z.clearOwner(owner)
					} else {
						z.clearOwnerType(owner, rtype)
					}
					cleared[key] = true
				}
			}
			z.add(rr)
		}

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

// loadActiveStacks returns the active, non-revoked stacks per tenant
// (keyed by tenant_id) with each one's activation timestamp. One query,
// fully drained before any per-zone work. Used to synthesize per-stack
// records and to feed the per-zone serial.
func loadActiveStacks(db *sql.DB) (map[string][]stackInfo, error) {
	rows, err := db.Query(`SELECT s.tenant_id, s.name, COALESCE(sv.activated_at, '')
	                          FROM stacks s
	                          JOIN stack_versions sv
	                            ON sv.stack_id = s.stack_id
	                           AND sv.version_number = s.active_version
	                         WHERE s.active_version IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("dns: query active stacks: %w", err)
	}
	defer rows.Close()
	out := map[string][]stackInfo{}
	for rows.Next() {
		var tid, name, act string
		if err := rows.Scan(&tid, &name, &act); err != nil {
			return nil, fmt.Errorf("dns: scan active stack: %w", err)
		}
		out[tid] = append(out[tid], stackInfo{name: name, activatedAt: act})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dns: iterate active stacks: %w", err)
	}
	return out, nil
}

// clearOwnerType drops the synthesized RRset for one (owner, type) so a
// materialized record can replace it. Leaves z.names intact — the owner
// still exists (the materialized record is added immediately after).
func (z *zone) clearOwnerType(ownerFQDN string, t uint16) {
	owner := strings.ToLower(ownerFQDN)
	if byType := z.rr[owner]; byType != nil {
		delete(byType, t)
	}
}

// clearOwner drops every RRset at an owner so a materialized CNAME can
// occlude the whole node. Leaves z.names intact (the CNAME is added
// immediately after).
func (z *zone) clearOwner(ownerFQDN string) {
	delete(z.rr, strings.ToLower(ownerFQDN))
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
// ANY is refused (no ANY expansion — anti-amplification). A name owning
// a CNAME answers every other qtype with that CNAME, followed in-zone
// (RFC 1034 §4.3.2(3a)); out-of-zone targets are the resolver's job.
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
		if cn := byType[dns.TypeCNAME]; len(cn) > 0 && q.Qtype != dns.TypeCNAME {
			return z.chaseCNAME(cn, q.Qtype), nil, dns.RcodeSuccess
		}
		// Name exists, requested type doesn't → NODATA.
		return nil, []dns.RR{z.soa}, dns.RcodeSuccess
	}
	if z.names[qname] {
		return nil, []dns.RR{z.soa}, dns.RcodeSuccess
	}
	// RFC 4592 wildcard: the queried name has no exact node, so synthesize from
	// the zone's `*.<origin>` RRset if present (the default-suffix zone's
	// wildcard A/MX/SPF). Answers carry the QUERIED name as owner — copy the
	// stored RR; never mutate the shared snapshot. Exact per-host/apex records
	// are matched above, so they always win. (Simplification: we don't model a
	// closer-encloser below an existing non-wildcard node — such queries don't
	// arise for structured hosts.)
	if byType, ok := z.rr["*."+z.originFQDN]; ok {
		if rrs := byType[q.Qtype]; len(rrs) > 0 {
			out := make([]dns.RR, 0, len(rrs))
			for _, rr := range rrs {
				cp := dns.Copy(rr)
				cp.Header().Name = qname
				out = append(out, cp)
			}
			return out, nil, dns.RcodeSuccess
		}
		if cn := byType[dns.TypeCNAME]; len(cn) > 0 && q.Qtype != dns.TypeCNAME {
			// Wildcard CNAME: synthesize with the queried name as owner,
			// then chase like an exact-match CNAME.
			cp := dns.Copy(cn[0])
			cp.Header().Name = qname
			return z.chaseCNAME([]dns.RR{cp}, q.Qtype), nil, dns.RcodeSuccess
		}
		// Wildcard owner exists but not this type → NODATA.
		return nil, []dns.RR{z.soa}, dns.RcodeSuccess
	}
	return nil, []dns.RR{z.soa}, dns.RcodeNameError
}

// chaseCNAME returns the CNAME RRs plus, while each target stays inside
// THIS zone, the target's RRset for qtype — following further in-zone
// CNAMEs up to a fixed depth (RFC 1034 §4.3.2(3a)). An out-of-zone
// target ends the chain: the resolver re-queries it (answering from a
// sibling served zone here would be out-of-bailiwick data most
// resolvers discard anyway). A missing in-zone target simply ends the
// answer — the resolver observes the dangling CNAME.
func (z *zone) chaseCNAME(start []dns.RR, qtype uint16) []dns.RR {
	out := append([]dns.RR(nil), start...)
	seen := map[string]bool{}
	cur := start[0]
	for depth := 0; depth < 8; depth++ {
		cn, ok := cur.(*dns.CNAME)
		if !ok {
			break
		}
		target := strings.ToLower(cn.Target)
		if seen[target] {
			break // loop guard
		}
		seen[target] = true
		if target != z.originFQDN && !strings.HasSuffix(target, "."+z.originFQDN) {
			break // out of zone
		}
		byType := z.rr[target]
		if byType == nil {
			break
		}
		if rrs := byType[qtype]; len(rrs) > 0 {
			out = append(out, rrs...)
			break
		}
		next := byType[dns.TypeCNAME]
		if len(next) == 0 {
			break
		}
		out = append(out, next...)
		cur = next[0]
	}
	return out
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

// Origins returns every canonical origin currently served, sorted. Used by
// the bundled cert manager to decide which `*.<origin>` + apex wildcard
// certificates to obtain/renew.
func (s *ZoneSnapshot) Origins() []string {
	out := make([]string, 0, len(s.zones))
	for _, z := range s.zones {
		out = append(out, z.origin)
	}
	sort.Strings(out)
	return out
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
