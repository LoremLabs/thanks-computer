package dns

import (
	"database/sql"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/miekg/dns"
	"go.uber.org/zap"

	dbpkg "github.com/loremlabs/thanks-computer/db"
)

// fixedTS is the updated_at stamped on the seeded zone + records, chosen
// so the per-zone serial is deterministic:
//
//	uint32(time.Parse(RFC3339, fixedTS).Unix()) == 1780065127
const (
	fixedTS       = "2026-05-29T14:32:07Z"
	fixedSerial   = uint32(1780065127)
	testTenantID  = "tnt_test"
	testOrigin    = "ops.example.com"
	otherTenantID = "tnt_other"
)

// newTestDB opens an in-memory SQLite DB with the 0011 schema applied,
// pinned to one connection (mirroring the chassis dbcache, which proves
// BuildSnapshot never holds two overlapping queries on the same conn).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// Apply ALL runtime migrations in order (not just 0011) so
	// cross-table reads — e.g. the active-stacks query feeding
	// synthesis — work against a realistic schema.
	entries, err := dbpkg.FS.ReadDir("schema/sqlite/runtime")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b, rerr := dbpkg.FS.ReadFile("schema/sqlite/runtime/" + n)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", n, rerr)
		}
		if _, eerr := db.Exec(string(b)); eerr != nil {
			t.Fatalf("apply %s: %v", n, eerr)
		}
	}
	return db
}

func seedZone(t *testing.T, db *sql.DB, ts string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO dns_zones
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, created_at, created_by, updated_at, verified_at)
		VALUES ('dz_1', ?, ?, 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, ?, 'actor_x', ?, ?)`,
		testTenantID, testOrigin, ts, ts, ts)
	if err != nil {
		t.Fatalf("insert zone: %v", err)
	}
	recs := []struct{ id, name, typ, rdata string }{
		{"dr_ns", "@", "NS", "ns1.txco.io."},
		{"dr_a", "@", "A", "192.0.2.10"},
		{"dr_mx", "@", "MX", "10 mail.ops.example.com."},
		{"dr_txt", "@", "TXT", "v=spf1 include:_spf.txco.io -all"},
		{"dr_www", "www", "A", "192.0.2.20"},
	}
	for _, r := range recs {
		_, err := db.Exec(`INSERT INTO dns_records
			(id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
			VALUES (?, 'dz_1', ?, ?, NULL, ?, ?, 'actor_x', ?)`,
			r.id, r.name, r.typ, r.rdata, ts, ts)
		if err != nil {
			t.Fatalf("insert record %s: %v", r.id, err)
		}
	}
}

func buildOrDie(t *testing.T, db *sql.DB, cfg SynthConfig) *ZoneSnapshot {
	t.Helper()
	snap, err := BuildSnapshot(db, cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	return snap
}

func q(name string, qtype uint16) dns.Question {
	return dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET}
}

func TestLookupMatrix(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	t.Run("SOA at apex", func(t *testing.T) {
		ans, ns, rcode := snap.Lookup(q("ops.example.com.", dns.TypeSOA))
		if rcode != dns.RcodeSuccess || len(ans) != 1 || len(ns) != 0 {
			t.Fatalf("got rcode=%d ans=%d ns=%d", rcode, len(ans), len(ns))
		}
		soa, ok := ans[0].(*dns.SOA)
		if !ok {
			t.Fatalf("answer not SOA: %T", ans[0])
		}
		if soa.Serial != fixedSerial {
			t.Fatalf("serial = %d, want %d", soa.Serial, fixedSerial)
		}
	})

	t.Run("NS / A / MX / TXT at apex", func(t *testing.T) {
		for _, tc := range []struct {
			qtype uint16
			want  string
		}{
			{dns.TypeNS, "ns1.txco.io."},
			{dns.TypeA, "192.0.2.10"},
			{dns.TypeMX, "mail.ops.example.com."},
			{dns.TypeTXT, "v=spf1"},
		} {
			ans, _, rcode := snap.Lookup(q("ops.example.com.", tc.qtype))
			if rcode != dns.RcodeSuccess || len(ans) != 1 {
				t.Fatalf("qtype %d: rcode=%d ans=%d", tc.qtype, rcode, len(ans))
			}
			if !strings.Contains(ans[0].String(), tc.want) {
				t.Fatalf("qtype %d: %q missing %q", tc.qtype, ans[0].String(), tc.want)
			}
		}
	})

	t.Run("sub-name A", func(t *testing.T) {
		ans, _, rcode := snap.Lookup(q("www.ops.example.com.", dns.TypeA))
		if rcode != dns.RcodeSuccess || len(ans) != 1 || !strings.Contains(ans[0].String(), "192.0.2.20") {
			t.Fatalf("www A: rcode=%d ans=%v", rcode, ans)
		}
	})

	t.Run("case-insensitive", func(t *testing.T) {
		ans, _, rcode := snap.Lookup(q("OPS.Example.COM.", dns.TypeSOA))
		if rcode != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("mixed-case SOA: rcode=%d ans=%d", rcode, len(ans))
		}
	})

	t.Run("NODATA — name exists, type absent", func(t *testing.T) {
		ans, ns, rcode := snap.Lookup(q("ops.example.com.", dns.TypeAAAA))
		if rcode != dns.RcodeSuccess || len(ans) != 0 || len(ns) != 1 {
			t.Fatalf("NODATA: rcode=%d ans=%d ns=%d", rcode, len(ans), len(ns))
		}
		if _, ok := ns[0].(*dns.SOA); !ok {
			t.Fatalf("NODATA authority not SOA: %T", ns[0])
		}
	})

	t.Run("NXDOMAIN — name absent", func(t *testing.T) {
		ans, ns, rcode := snap.Lookup(q("nope.ops.example.com.", dns.TypeA))
		if rcode != dns.RcodeNameError || len(ans) != 0 || len(ns) != 1 {
			t.Fatalf("NXDOMAIN: rcode=%d ans=%d ns=%d", rcode, len(ans), len(ns))
		}
		if _, ok := ns[0].(*dns.SOA); !ok {
			t.Fatalf("NXDOMAIN authority not SOA: %T", ns[0])
		}
	})

	t.Run("REFUSED — not our zone", func(t *testing.T) {
		ans, ns, rcode := snap.Lookup(q("example.org.", dns.TypeA))
		if rcode != dns.RcodeRefused || len(ans) != 0 || len(ns) != 0 {
			t.Fatalf("REFUSED: rcode=%d ans=%d ns=%d", rcode, len(ans), len(ns))
		}
	})

	t.Run("ANY refused (no amplification)", func(t *testing.T) {
		_, _, rcode := snap.Lookup(q("ops.example.com.", dns.TypeANY))
		if rcode != dns.RcodeRefused {
			t.Fatalf("ANY: rcode=%d, want REFUSED", rcode)
		}
	})
}

func TestSerialIsContentDerived(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)

	s1 := buildOrDie(t, db, SynthConfig{}).zoneSerial(t)
	// Rebuild with no change → identical serial (no churn).
	s2 := buildOrDie(t, db, SynthConfig{}).zoneSerial(t)
	if s1 != s2 {
		t.Fatalf("no-op rebuild changed serial: %d -> %d", s1, s2)
	}
	if s1 != fixedSerial {
		t.Fatalf("serial = %d, want %d", s1, fixedSerial)
	}

	// A real content change (later updated_at) advances the serial.
	if _, err := db.Exec(`UPDATE dns_records SET rdata='192.0.2.99', updated_at='2026-06-01T00:00:00Z' WHERE id='dr_a'`); err != nil {
		t.Fatalf("update record: %v", err)
	}
	s3 := buildOrDie(t, db, SynthConfig{}).zoneSerial(t)
	if s3 <= s1 {
		t.Fatalf("serial did not advance after change: %d -> %d", s1, s3)
	}
}

// zoneSerial is a test helper that pulls the single seeded zone's serial.
func (s *ZoneSnapshot) zoneSerial(t *testing.T) uint32 {
	t.Helper()
	z := s.byOrigin(testOrigin)
	if z == nil {
		t.Fatalf("zone %s not in snapshot", testOrigin)
	}
	return z.serial
}

func TestRenderGolden(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	got, ok := snap.Render(testOrigin)
	if !ok {
		t.Fatalf("Render(%s) not ok", testOrigin)
	}

	// Whitespace-normalized golden: tolerant of tab-vs-space in RR
	// presentation, strict on content + ordering + the serial/generation
	// stamp. Records sort by owner name, then rrtype (A<NS<MX<TXT), so
	// the apex block is A, NS, MX, TXT before the www record; the
	// synthesized SOA is emitted right after the header comment.
	want := strings.Join([]string{
		"; zone ops.example.com — generation 2026-05-29T14:32:07Z (serial 1780065127)",
		"ops.example.com. 300 IN SOA ns1.txco.io. hostmaster.txco.io. 1780065127 7200 3600 1209600 300",
		"ops.example.com. 300 IN A 192.0.2.10",
		"ops.example.com. 300 IN NS ns1.txco.io.",
		"ops.example.com. 300 IN MX 10 mail.ops.example.com.",
		`ops.example.com. 300 IN TXT "v=spf1 include:_spf.txco.io -all"`,
		"www.ops.example.com. 300 IN A 192.0.2.20",
	}, "\n")

	if normalizeZone(got) != normalizeZone(want) {
		t.Fatalf("render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	if _, ok := snap.Render("not-served.example.net"); ok {
		t.Fatalf("Render of unserved origin returned ok=true")
	}
}

// normalizeZone collapses each line's runs of whitespace to a single
// space and drops blank lines, so a golden compare ignores the
// tab-vs-space details of dns.RR presentation.
func normalizeZone(s string) string {
	var lines []string
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		if f := strings.Fields(ln); len(f) > 0 {
			lines = append(lines, strings.Join(f, " "))
		}
	}
	return strings.Join(lines, "\n")
}

func TestMalformedRecordSkipped(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	// A bogus A record must be skipped, not fatal — the rest of the zone
	// still serves.
	if _, err := db.Exec(`INSERT INTO dns_records (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		VALUES ('dr_bad', 'dz_1', 'bad', 'A', NULL, 'not-an-ip', ?, 'actor_x', ?)`, fixedTS, fixedTS); err != nil {
		t.Fatalf("insert bad record: %v", err)
	}
	snap := buildOrDie(t, db, SynthConfig{}) // must not error
	// The bad name resolves to NXDOMAIN (it never made it into the zone).
	if _, _, rcode := snap.Lookup(q("bad.ops.example.com.", dns.TypeA)); rcode != dns.RcodeNameError {
		t.Fatalf("bad record leaked: rcode=%d", rcode)
	}
	// The good records still answer.
	if ans, _, rcode := snap.Lookup(q("ops.example.com.", dns.TypeA)); rcode != dns.RcodeSuccess || len(ans) != 1 {
		t.Fatalf("good A broken by bad sibling: rcode=%d ans=%d", rcode, len(ans))
	}
}

func TestTenantScoping(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	// A second zone owned by a different tenant.
	if _, err := db.Exec(`INSERT INTO dns_zones
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, created_at, created_by, updated_at, verified_at)
		VALUES ('dz_2', ?, 'other.example.net', 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, ?, 'actor_y', ?, ?)`,
		otherTenantID, fixedTS, fixedTS, fixedTS); err != nil {
		t.Fatalf("insert other zone: %v", err)
	}
	snap := buildOrDie(t, db, SynthConfig{})

	if got := snap.OriginsForTenant(testTenantID); len(got) != 1 || got[0] != testOrigin {
		t.Fatalf("OriginsForTenant(test) = %v", got)
	}
	if got := snap.OriginsForTenant(otherTenantID); len(got) != 1 || got[0] != "other.example.net" {
		t.Fatalf("OriginsForTenant(other) = %v", got)
	}
}

// TestLookupCNAME covers the CNAME answer semantics: any qtype at a
// CNAME owner answers with the CNAME, chased while targets stay in-zone
// (RFC 1034 §4.3.2(3a)); out-of-zone targets and loops end the chain.
func TestLookupCNAME(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	cnames := []struct{ id, name, rdata string }{
		{"dr_cn_blog", "blog", "www.ops.example.com."},
		{"dr_cn_alias", "alias", "blog.ops.example.com."},
		{"dr_cn_ext", "ext", "driplit.github.io."},
		{"dr_cn_loop1", "loop1", "loop2.ops.example.com."},
		{"dr_cn_loop2", "loop2", "loop1.ops.example.com."},
	}
	for _, r := range cnames {
		if _, err := db.Exec(`INSERT INTO dns_records
			(id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
			VALUES (?, 'dz_1', ?, 'CNAME', NULL, ?, ?, 'actor_x', ?)`,
			r.id, r.name, r.rdata, fixedTS, fixedTS); err != nil {
			t.Fatalf("insert cname %s: %v", r.id, err)
		}
	}
	snap := buildOrDie(t, db, SynthConfig{})

	t.Run("A query at CNAME owner chases to the in-zone A", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("blog.ops.example.com.", dns.TypeA))
		if rc != dns.RcodeSuccess || len(ans) != 2 {
			t.Fatalf("rc=%d ans=%v", rc, ans)
		}
		if _, ok := ans[0].(*dns.CNAME); !ok {
			t.Fatalf("first RR not CNAME: %T", ans[0])
		}
		if a, ok := ans[1].(*dns.A); !ok || a.A.String() != "192.0.2.20" {
			t.Fatalf("chased A wrong: %v", ans[1])
		}
	})
	t.Run("CNAME qtype answers just the CNAME", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("blog.ops.example.com.", dns.TypeCNAME))
		if rc != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("rc=%d ans=%v", rc, ans)
		}
	})
	t.Run("two-link in-zone chain", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("alias.ops.example.com.", dns.TypeA))
		if rc != dns.RcodeSuccess || len(ans) != 3 {
			t.Fatalf("rc=%d ans=%v", rc, ans)
		}
		if a, ok := ans[2].(*dns.A); !ok || a.A.String() != "192.0.2.20" {
			t.Fatalf("chain tail wrong: %v", ans[2])
		}
	})
	t.Run("out-of-zone target is left to the resolver", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("ext.ops.example.com.", dns.TypeA))
		if rc != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("rc=%d ans=%v", rc, ans)
		}
		if cn, ok := ans[0].(*dns.CNAME); !ok || cn.Target != "driplit.github.io." {
			t.Fatalf("CNAME wrong: %v", ans[0])
		}
	})
	t.Run("CNAME loop terminates", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("loop1.ops.example.com.", dns.TypeA))
		if rc != dns.RcodeSuccess || len(ans) < 1 || len(ans) > 3 {
			t.Fatalf("loop not bounded: rc=%d ans=%v", rc, ans)
		}
	})
	t.Run("qtype absent at chased target ends the answer at the CNAME", func(t *testing.T) {
		ans, _, rc := snap.Lookup(q("blog.ops.example.com.", dns.TypeTXT))
		if rc != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("rc=%d ans=%v", rc, ans)
		}
		if _, ok := ans[0].(*dns.CNAME); !ok {
			t.Fatalf("not CNAME: %T", ans[0])
		}
	})
}

// TestCNAMEOverrideOccludesSynth: in a pattern zone a materialized CNAME
// must clear EVERY synthesized type at its owner, not just (owner,CNAME)
// — a synthesized A left beside it would serve an illegal node.
func TestCNAMEOverrideOccludesSynth(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	seedActiveStack(t, db, patTenant, "shop", fixedTS)
	if _, err := db.Exec(`INSERT INTO dns_records
		(id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		VALUES ('dr_cn_shop', 'dz_pat', 'shop', 'CNAME', NULL, 'edge.pat.example.com.', ?, 'actor_x', ?)`,
		fixedTS, fixedTS); err != nil {
		t.Fatalf("insert cname: %v", err)
	}
	snap := buildOrDie(t, db, patCfg())

	ans, _, rc := snap.Lookup(q("shop.pat.example.com.", dns.TypeA))
	if rc != dns.RcodeSuccess || len(ans) == 0 {
		t.Fatalf("rc=%d ans=%v", rc, ans)
	}
	if _, ok := ans[0].(*dns.CNAME); !ok {
		t.Fatalf("first RR must be the CNAME override, got %T (synthesized A not occluded?)", ans[0])
	}
	for _, rr := range ans {
		if a, ok := rr.(*dns.A); ok && a.Header().Name == "shop.pat.example.com." {
			t.Fatalf("synthesized A survived beside the CNAME: %v", ans)
		}
	}
}
