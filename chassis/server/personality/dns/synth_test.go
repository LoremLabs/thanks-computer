package dns

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func TestSynthesisMailAuth(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	cfg := patCfg()
	cfg.DMARC = "v=DMARC1; p=none"
	snap := buildOrDie(t, db, cfg)

	t.Run("apex SPF auto-derived from edge IPs + mx", func(t *testing.T) {
		txt, _, rc := snap.Lookup(q("pat.example.com.", dns.TypeTXT))
		if rc != dns.RcodeSuccess || len(txt) != 1 {
			t.Fatalf("apex TXT: rc=%d n=%d", rc, len(txt))
		}
		if got := strings.Join(txt[0].(*dns.TXT).Txt, ""); got != "v=spf1 ip4:203.0.113.10 mx ~all" {
			t.Fatalf("SPF = %q", got)
		}
	})
	t.Run("DMARC at _dmarc", func(t *testing.T) {
		txt, _, rc := snap.Lookup(q("_dmarc.pat.example.com.", dns.TypeTXT))
		if rc != dns.RcodeSuccess || len(txt) != 1 ||
			strings.Join(txt[0].(*dns.TXT).Txt, "") != "v=DMARC1; p=none" {
			t.Fatalf("DMARC: rc=%d %v", rc, txt)
		}
	})
	t.Run("no MX host → no SPF/DMARC", func(t *testing.T) {
		c2 := patCfg()
		c2.MXHost = ""
		c2.DMARC = "v=DMARC1; p=none"
		snap2 := buildOrDie(t, db, c2)
		if txt, _, _ := snap2.Lookup(q("pat.example.com.", dns.TypeTXT)); len(txt) != 0 {
			t.Fatalf("SPF emitted without MX: %v", txt)
		}
	})
}

const patTenant = "tnt_pat"

// seedPatternZone inserts a pattern-mode delegated zone with NO
// materialized records — synthesis drives it entirely.
func seedPatternZone(t *testing.T, db *sql.DB, tenantID, origin, ts string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO dns_zones
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, mode, created_at, created_by, updated_at, verified_at)
		VALUES ('dz_pat', ?, ?, 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, 'pattern', ?, 'seed', ?, ?)`,
		tenantID, origin, ts, ts, ts)
	if err != nil {
		t.Fatalf("insert pattern zone: %v", err)
	}
}

// seedActiveStack inserts an active stack (active_version → a
// stack_versions row carrying activated_at) for a tenant. Mirrors the
// real activation write: `stacks.active_version` is the version row's
// version_id (global PK), not its per-stack version_number — the second
// seeded stack therefore points at version_id 2 while its
// version_number is still 1. (The old helper hard-coded active_version=1
// for every stack, which is exactly the coincidence that hid the
// version_number join bug in loadActiveStacks.)
func seedActiveStack(t *testing.T, db *sql.DB, tenantID, name, activatedAt string) {
	t.Helper()
	sid := "stk_" + name
	if _, err := db.Exec(`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		VALUES (?, ?, ?, NULL, ?)`, sid, tenantID, name, activatedAt); err != nil {
		t.Fatalf("insert stack: %v", err)
	}
	res, err := db.Exec(`INSERT INTO stack_versions
		(stack_id, version_number, status, created_by, created_at, activated_at)
		VALUES (?, 1, 'draft', 'seed', ?, ?)`, sid, activatedAt, activatedAt)
	if err != nil {
		t.Fatalf("insert stack_version: %v", err)
	}
	vid, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("stack_version id: %v", err)
	}
	if _, err := db.Exec(`UPDATE stacks SET active_version = ? WHERE stack_id = ?`, vid, sid); err != nil {
		t.Fatalf("activate stack: %v", err)
	}
}

// TestLoadActiveStacksJoinsOnVersionID pins the join column: a stack whose
// active version_id (42) differs from its version_number (7) must still be
// found, and a dangling active_version must not. Regression for the
// version_number join that made per-stack host synthesis (and the observe
// subscription) silently miss every stack past the first few on a chassis.
func TestLoadActiveStacksJoinsOnVersionID(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(`INSERT INTO stack_versions
		(version_id, stack_id, version_number, status, created_by, created_at, activated_at)
		VALUES (42, 'stk_api', 7, 'draft', 'seed', ?, ?)`, fixedTS, fixedTS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		VALUES ('stk_api', ?, 'api', 42, ?), ('stk_gone', ?, 'gone', 999, ?)`,
		patTenant, fixedTS, patTenant, fixedTS); err != nil {
		t.Fatal(err)
	}
	got, err := loadActiveStacks(db)
	if err != nil {
		t.Fatal(err)
	}
	stacks := got[patTenant]
	if len(stacks) != 1 || stacks[0].name != "api" || stacks[0].activatedAt != fixedTS {
		t.Fatalf("active stacks = %+v, want only api@%s", stacks, fixedTS)
	}
}

func patCfg() SynthConfig {
	return SynthConfig{
		Nameservers: []string{"ns1.txco.io", "ns2.txco.io"},
		EdgeIPs:     []string{"203.0.113.10"},
		MXHost:      "mx.txco.io",
		MXPriority:  10,
		TTL:         300,
	}
}

func TestSynthesisWildcardSuffix(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "stacks.example.com", fixedTS)
	// A structured host with its own per-host DKIM public key.
	if _, err := db.Exec(`INSERT INTO tenant_hostnames
		(id, hostname, tenant_id, stack, created_at, created_by, verified_at, dkim_selector, dkim_public_b64)
		VALUES ('h_x','web-x.stacks.example.com', ?, 'web', ?, 'system:structured-host', ?, 'txco', 'PUBKEYB64')`,
		patTenant, fixedTS, fixedTS); err != nil {
		t.Fatalf("seed structured host: %v", err)
	}
	cfg := patCfg()
	cfg.StructuredSuffix = "stacks.example.com"
	cfg.DMARC = "v=DMARC1; p=none"
	snap := buildOrDie(t, db, cfg)

	t.Run("wildcard A carries the queried name", func(t *testing.T) {
		a, _, rc := snap.Lookup(q("foo-rand.stacks.example.com.", dns.TypeA))
		if rc != dns.RcodeSuccess || len(a) != 1 || a[0].(*dns.A).A.String() != "203.0.113.10" {
			t.Fatalf("wildcard A: rc=%d %v", rc, a)
		}
		if a[0].Header().Name != "foo-rand.stacks.example.com." {
			t.Fatalf("owner must be the queried name, got %s", a[0].Header().Name)
		}
	})
	t.Run("wildcard MX + SPF", func(t *testing.T) {
		mx, _, _ := snap.Lookup(q("foo-rand.stacks.example.com.", dns.TypeMX))
		if len(mx) != 1 || mx[0].(*dns.MX).Mx != "mx.txco.io." {
			t.Fatalf("wildcard MX: %v", mx)
		}
		txt, _, _ := snap.Lookup(q("foo-rand.stacks.example.com.", dns.TypeTXT))
		if len(txt) != 1 || !strings.Contains(strings.Join(txt[0].(*dns.TXT).Txt, ""), "v=spf1") {
			t.Fatalf("wildcard SPF: %v", txt)
		}
	})
	t.Run("multi-label name still wildcards", func(t *testing.T) {
		if a, _, rc := snap.Lookup(q("a.b.stacks.example.com.", dns.TypeA)); rc != dns.RcodeSuccess || len(a) != 1 {
			t.Fatalf("multi-label A: rc=%d %v", rc, a)
		}
	})
	t.Run("per-host DKIM is exact (wins over wildcard)", func(t *testing.T) {
		txt, _, rc := snap.Lookup(q("txco._domainkey.web-x.stacks.example.com.", dns.TypeTXT))
		if rc != dns.RcodeSuccess || len(txt) != 1 ||
			strings.Join(txt[0].(*dns.TXT).Txt, "") != "v=DKIM1; k=rsa; p=PUBKEYB64" {
			t.Fatalf("per-host DKIM: rc=%d %v", rc, txt)
		}
	})
	t.Run("per-host DMARC", func(t *testing.T) {
		txt, _, _ := snap.Lookup(q("_dmarc.web-x.stacks.example.com.", dns.TypeTXT))
		if len(txt) != 1 || strings.Join(txt[0].(*dns.TXT).Txt, "") != "v=DMARC1; p=none" {
			t.Fatalf("per-host DMARC: %v", txt)
		}
	})
}

func TestSynthesisDKIM(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	if _, err := db.Exec(`UPDATE dns_zones SET dkim_selector='txco', dkim_public_b64='PUBKEYB64'
		WHERE origin='pat.example.com'`); err != nil {
		t.Fatalf("set dkim key: %v", err)
	}
	snap := buildOrDie(t, db, patCfg())
	txt, _, rc := snap.Lookup(q("txco._domainkey.pat.example.com.", dns.TypeTXT))
	if rc != dns.RcodeSuccess || len(txt) != 1 {
		t.Fatalf("DKIM TXT: rc=%d n=%d", rc, len(txt))
	}
	if got := strings.Join(txt[0].(*dns.TXT).Txt, ""); got != "v=DKIM1; k=rsa; p=PUBKEYB64" {
		t.Fatalf("DKIM TXT = %q", got)
	}
}

// A pattern zone with no DKIM key (the default) publishes no _domainkey TXT.
func TestSynthesisNoDKIMWithoutKey(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	snap := buildOrDie(t, db, patCfg())
	if _, _, rc := snap.Lookup(q("txco._domainkey.pat.example.com.", dns.TypeTXT)); rc != dns.RcodeNameError {
		t.Fatalf("keyless zone leaked a DKIM record: rc=%d", rc)
	}
}

func TestSynthesisPattern(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	seedActiveStack(t, db, patTenant, "web-api", fixedTS)
	// A system stack must NOT be synthesized.
	seedActiveStack(t, db, patTenant, "_sys", fixedTS)
	snap := buildOrDie(t, db, patCfg())

	t.Run("apex NS synthesized", func(t *testing.T) {
		ans, _, rcode := snap.Lookup(q("pat.example.com.", dns.TypeNS))
		if rcode != dns.RcodeSuccess || len(ans) != 2 {
			t.Fatalf("apex NS: rcode=%d ans=%d", rcode, len(ans))
		}
	})
	t.Run("apex A + MX synthesized", func(t *testing.T) {
		a, _, _ := snap.Lookup(q("pat.example.com.", dns.TypeA))
		if len(a) != 1 || a[0].(*dns.A).A.String() != "203.0.113.10" {
			t.Fatalf("apex A: %v", a)
		}
		mx, _, _ := snap.Lookup(q("pat.example.com.", dns.TypeMX))
		if len(mx) != 1 || mx[0].(*dns.MX).Mx != "mx.txco.io." {
			t.Fatalf("apex MX: %v", mx)
		}
	})
	t.Run("per-stack host synthesized by substitution", func(t *testing.T) {
		a, _, rcode := snap.Lookup(q("web-api.pat.example.com.", dns.TypeA))
		if rcode != dns.RcodeSuccess || len(a) != 1 || a[0].(*dns.A).A.String() != "203.0.113.10" {
			t.Fatalf("stack A: rcode=%d %v", rcode, a)
		}
		mx, _, _ := snap.Lookup(q("web-api.pat.example.com.", dns.TypeMX))
		if len(mx) != 1 || mx[0].(*dns.MX).Mx != "mx.txco.io." {
			t.Fatalf("stack MX: %v", mx)
		}
	})
	t.Run("system stack not synthesized", func(t *testing.T) {
		_, _, rcode := snap.Lookup(q("-sys.pat.example.com.", dns.TypeA))
		if rcode != dns.RcodeNameError {
			t.Fatalf("_sys leaked: rcode=%d", rcode)
		}
	})
}

func TestMaterializedOverridesSynthesis(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	// Override the apex A with a materialized record; NS stays synthesized.
	if _, err := db.Exec(`INSERT INTO dns_records (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		VALUES ('dr_ov', 'dz_pat', '@', 'A', NULL, '198.51.100.7', ?, 'op', ?)`, fixedTS, fixedTS); err != nil {
		t.Fatalf("insert override: %v", err)
	}
	snap := buildOrDie(t, db, patCfg())

	a, _, _ := snap.Lookup(q("pat.example.com.", dns.TypeA))
	if len(a) != 1 || a[0].(*dns.A).A.String() != "198.51.100.7" {
		t.Fatalf("materialized A did not win: %v", a)
	}
	if ns, _, _ := snap.Lookup(q("pat.example.com.", dns.TypeNS)); len(ns) != 2 {
		t.Fatalf("NS should stay synthesized: %d", len(ns))
	}
}

func TestManualModeNoSynthesis(t *testing.T) {
	db := newTestDB(t)
	// manual zone with a single explicit A; synthesis must not add NS/MX.
	if _, err := db.Exec(`INSERT INTO dns_zones
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, mode, created_at, created_by, updated_at, verified_at)
		VALUES ('dz_man', ?, 'man.example.com', 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, 'manual', ?, 'seed', ?, ?)`,
		patTenant, fixedTS, fixedTS, fixedTS); err != nil {
		t.Fatalf("insert manual zone: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO dns_records (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		VALUES ('dr_man', 'dz_man', '@', 'A', NULL, '192.0.2.1', ?, 'op', ?)`, fixedTS, fixedTS); err != nil {
		t.Fatalf("insert manual record: %v", err)
	}
	seedActiveStack(t, db, patTenant, "web", fixedTS)
	snap := buildOrDie(t, db, patCfg())

	if a, _, _ := snap.Lookup(q("man.example.com.", dns.TypeA)); len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatalf("manual A: %v", a)
	}
	// No synthesized NS/MX, and no per-stack host in a manual zone.
	if _, _, rcode := snap.Lookup(q("man.example.com.", dns.TypeNS)); rcode != dns.RcodeSuccess {
		// NS absent → NODATA (NOERROR, no answer). Confirm no NS records.
		t.Fatalf("unexpected NS rcode in manual zone: %d", rcode)
	}
	if ns, _, _ := snap.Lookup(q("man.example.com.", dns.TypeNS)); len(ns) != 0 {
		t.Fatalf("manual zone should have no synthesized NS: %d", len(ns))
	}
	if _, _, rcode := snap.Lookup(q("web.man.example.com.", dns.TypeA)); rcode != dns.RcodeNameError {
		t.Fatalf("manual zone synthesized a per-stack host: rcode=%d", rcode)
	}
}

// seedSettings inserts the singleton dns_settings row.
func seedSettings(t *testing.T, db *sql.DB, ns, edge, mx string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO dns_settings
		(singleton,nameservers,edge_ips,mx_host,mx_priority,synth_ttl,updated_at)
		VALUES (1,?,?,?,10,300,?)`, ns, edge, mx, fixedTS); err != nil {
		t.Fatalf("insert dns_settings: %v", err)
	}
}

func TestEffectiveSynthConfig(t *testing.T) {
	db := newTestDB(t)
	flags := SynthConfig{Nameservers: []string{"flag-ns.example."}, EdgeIPs: []string{"192.0.2.1"}, MXHost: "flag-mx."}

	// No settings row → flag defaults.
	if got := EffectiveSynthConfig(db, flags); len(got.Nameservers) != 1 || got.Nameservers[0] != "flag-ns.example." {
		t.Fatalf("no row should use flags: %+v", got)
	}

	// Row present → row wins entirely.
	seedSettings(t, db, "ns1.txco.io,ns2.txco.io", "203.0.113.10", "mx.txco.io")
	got := EffectiveSynthConfig(db, flags)
	if len(got.Nameservers) != 2 || got.EdgeIPs[0] != "203.0.113.10" || got.MXHost != "mx.txco.io" {
		t.Fatalf("settings row should win: %+v", got)
	}
}

// TestSettingsDriveSynthesis proves the operator-set settings (not boot
// flags) parameterize synthesis: flag defaults are EMPTY here, yet the
// pattern is fully populated from the dns_settings row.
func TestSettingsDriveSynthesis(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	seedActiveStack(t, db, patTenant, "web-api", fixedTS)
	seedSettings(t, db, "ns1.txco.io", "203.0.113.10", "mx.txco.io")

	snap := buildOrDie(t, db, SynthConfig{}) // empty flag defaults

	if a, _, _ := snap.Lookup(q("web-api.pat.example.com.", dns.TypeA)); len(a) != 1 || a[0].(*dns.A).A.String() != "203.0.113.10" {
		t.Fatalf("settings did not drive per-stack A: %v", a)
	}
	if mx, _, _ := snap.Lookup(q("web-api.pat.example.com.", dns.TypeMX)); len(mx) != 1 || mx[0].(*dns.MX).Mx != "mx.txco.io." {
		t.Fatalf("settings did not drive per-stack MX: %v", mx)
	}
	if ns, _, _ := snap.Lookup(q("pat.example.com.", dns.TypeNS)); len(ns) != 1 {
		t.Fatalf("settings did not drive apex NS: %d", len(ns))
	}
}

func TestSerialReflectsStackActivation(t *testing.T) {
	db := newTestDB(t)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	base := buildOrDie(t, db, patCfg()).byOrigin("pat.example.com").serial

	// Activating a stack later than the zone's own updated_at must bump
	// the (content-derived) serial.
	seedActiveStack(t, db, patTenant, "web", "2026-06-10T00:00:00Z")
	after := buildOrDie(t, db, patCfg()).byOrigin("pat.example.com").serial
	if after <= base {
		t.Fatalf("serial did not advance after stack activation: %d -> %d", base, after)
	}
}

// TestSynthesisIMAPSSRV covers the RFC 6186 discovery record: at the apex
// and every per-stack host of a pattern zone (target = the name itself),
// at the default-suffix wildcard (target = imap.<suffix>, the one name a
// wildcard can point at), and absent when the port is 0.
func TestSynthesisIMAPSSRV(t *testing.T) {
	srv := func(t *testing.T, snap *ZoneSnapshot, name string) *dns.SRV {
		t.Helper()
		ans, _, rc := snap.Lookup(q(name, dns.TypeSRV))
		if rc != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("%s SRV: rc=%d ans=%v", name, rc, ans)
		}
		rr, ok := ans[0].(*dns.SRV)
		if !ok {
			t.Fatalf("%s: not an SRV: %T", name, ans[0])
		}
		if rr.Header().Name != name {
			t.Fatalf("%s: owner must be the queried name, got %s", name, rr.Header().Name)
		}
		return rr
	}

	t.Run("pattern zone: apex + per-stack, target = the host", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
		seedActiveStack(t, db, patTenant, "web-api", fixedTS)
		cfg := patCfg()
		cfg.IMAPSPort = 993
		snap := buildOrDie(t, db, cfg)

		apex := srv(t, snap, "_imaps._tcp.pat.example.com.")
		if apex.Port != 993 || apex.Target != "pat.example.com." || apex.Priority != 0 || apex.Weight != 1 {
			t.Fatalf("apex SRV: %v", apex)
		}
		host := srv(t, snap, "_imaps._tcp.web-api.pat.example.com.")
		if host.Port != 993 || host.Target != "web-api.pat.example.com." {
			t.Fatalf("stack SRV: %v", host)
		}
		// The SRV target resolves in the same zone.
		if a, _, rc := snap.Lookup(q(host.Target, dns.TypeA)); rc != dns.RcodeSuccess || len(a) != 1 {
			t.Fatalf("SRV target A: rc=%d %v", rc, a)
		}
	})

	t.Run("wildcard suffix zone: one target under the wildcard", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "stacks.example.com", fixedTS)
		cfg := patCfg()
		cfg.StructuredSuffix = "stacks.example.com"
		cfg.IMAPSPort = 993
		snap := buildOrDie(t, db, cfg)

		for _, name := range []string{"_imaps._tcp.foo-rand.stacks.example.com.", "_imaps._tcp.a.b.stacks.example.com."} {
			rr := srv(t, snap, name)
			if rr.Port != 993 || rr.Target != "imap.stacks.example.com." {
				t.Fatalf("%s: %v", name, rr)
			}
		}
		// The shared target resolves through the wildcard A.
		if a, _, rc := snap.Lookup(q("imap.stacks.example.com.", dns.TypeA)); rc != dns.RcodeSuccess || len(a) != 1 {
			t.Fatalf("imap.<suffix> A: rc=%d %v", rc, a)
		}
		// The apex still gets its own explicit record.
		if rr := srv(t, snap, "_imaps._tcp.stacks.example.com."); rr.Target != "stacks.example.com." {
			t.Fatalf("apex SRV: %v", rr)
		}
	})

	t.Run("off by default", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
		seedActiveStack(t, db, patTenant, "web-api", fixedTS)
		snap := buildOrDie(t, db, patCfg())
		for _, name := range []string{"_imaps._tcp.pat.example.com.", "_imaps._tcp.web-api.pat.example.com."} {
			if ans, _, _ := snap.Lookup(q(name, dns.TypeSRV)); len(ans) != 0 {
				t.Fatalf("%s: SRV emitted with port 0: %v", name, ans)
			}
		}
	})
}

func TestSynthCalDAVSRV(t *testing.T) {
	srv := func(t *testing.T, snap *ZoneSnapshot, name string) *dns.SRV {
		t.Helper()
		ans, _, rc := snap.Lookup(q(name, dns.TypeSRV))
		if rc != dns.RcodeSuccess || len(ans) != 1 {
			t.Fatalf("%s SRV: rc=%d ans=%v", name, rc, ans)
		}
		rr, ok := ans[0].(*dns.SRV)
		if !ok {
			t.Fatalf("%s: not an SRV: %T", name, ans[0])
		}
		return rr
	}
	t.Run("pattern zone: SRV + path TXT at apex and per stack", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
		seedActiveStack(t, db, patTenant, "web-api", fixedTS)
		cfg := patCfg()
		cfg.CalDAVSPort = 443
		snap := buildOrDie(t, db, cfg)
		for _, name := range []string{"_caldavs._tcp.pat.example.com.", "_caldavs._tcp.web-api.pat.example.com."} {
			rr := srv(t, snap, name)
			want := strings.TrimPrefix(name, "_caldavs._tcp.")
			if rr.Port != 443 || rr.Target != want || rr.Priority != 0 || rr.Weight != 1 {
				t.Fatalf("%s SRV: %v", name, rr)
			}
			txt, _, rc := snap.Lookup(q(name, dns.TypeTXT))
			if rc != dns.RcodeSuccess || len(txt) != 1 || !strings.Contains(txt[0].String(), "path=/.well-known/caldav") {
				t.Fatalf("%s TXT: rc=%d %v", name, rc, txt)
			}
		}
	})
	t.Run("wildcard suffix zone: no caldav SRV under the wildcard", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "stacks.example.com", fixedTS)
		cfg := patCfg()
		cfg.StructuredSuffix = "stacks.example.com"
		cfg.IMAPSPort = 993
		cfg.CalDAVSPort = 443
		snap := buildOrDie(t, db, cfg)
		// The wildcard SRV answers ONE service: a `_caldavs._tcp` question
		// under it is NODATA (the client falls back to /.well-known/caldav),
		// never the IMAPS record it cannot tell apart — that misdirection
		// sent Apple Calendar to port 993 on prod.
		ans, auth, rc := snap.Lookup(q("_caldavs._tcp.foo-rand.stacks.example.com.", dns.TypeSRV))
		if rc != dns.RcodeSuccess || len(ans) != 0 || len(auth) != 1 {
			t.Fatalf("wildcard caldav SRV: rc=%d ans=%v auth=%v (want NODATA)", rc, ans, auth)
		}
		if rr := srv(t, snap, "_imaps._tcp.foo-rand.stacks.example.com."); rr.Port != 993 {
			t.Fatalf("wildcard imaps SRV: %v", rr)
		}
		// A also still resolves through the wildcard.
		if a, _, rc := snap.Lookup(q("foo-rand.stacks.example.com.", dns.TypeA)); rc != dns.RcodeSuccess || len(a) != 1 {
			t.Fatalf("wildcard A: rc=%d %v", rc, a)
		}
		// The apex has its explicit pair.
		if rr := srv(t, snap, "_caldavs._tcp.stacks.example.com."); rr.Port != 443 || rr.Target != "stacks.example.com." {
			t.Fatalf("apex SRV: %v", rr)
		}
	})
	t.Run("structured host: exact SRVs for both services beat the wildcard", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "stacks.example.com", fixedTS)
		if _, err := db.Exec(`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, created_by, dkim_selector, dkim_public_b64)
			VALUES ('h_c','core-abc.stacks.example.com', ?, 'core', ?, 'system:structured-host', '', '')`, patTenant, fixedTS); err != nil {
			t.Fatalf("seed structured host: %v", err)
		}
		cfg := patCfg()
		cfg.StructuredSuffix = "stacks.example.com"
		cfg.IMAPSPort = 993
		cfg.CalDAVSPort = 443
		snap := buildOrDie(t, db, cfg)
		if rr := srv(t, snap, "_caldavs._tcp.core-abc.stacks.example.com."); rr.Port != 443 || rr.Target != "core-abc.stacks.example.com." {
			t.Fatalf("host caldav SRV: %v", rr)
		}
		if rr := srv(t, snap, "_imaps._tcp.core-abc.stacks.example.com."); rr.Port != 993 || rr.Target != "core-abc.stacks.example.com." {
			t.Fatalf("host imaps SRV: %v (must be the host, not imap.<suffix>)", rr)
		}
		txt, _, rc := snap.Lookup(q("_caldavs._tcp.core-abc.stacks.example.com.", dns.TypeTXT))
		if rc != dns.RcodeSuccess || len(txt) != 1 || !strings.Contains(txt[0].String(), "path=/.well-known/caldav") {
			t.Fatalf("host caldav TXT: rc=%d %v", rc, txt)
		}
		// No DKIM key ⇒ no _domainkey TXT (the SRVs exist regardless).
		if a, _, _ := snap.Lookup(q("txco._domainkey.core-abc.stacks.example.com.", dns.TypeTXT)); len(a) != 0 {
			for _, rr := range a {
				if strings.Contains(rr.String(), "DKIM1") {
					t.Fatalf("unexpected DKIM for a host without a key: %v", a)
				}
			}
		}
	})

	t.Run("off by default", func(t *testing.T) {
		db := newTestDB(t)
		seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
		snap := buildOrDie(t, db, patCfg())
		if ans, _, _ := snap.Lookup(q("_caldavs._tcp.pat.example.com.", dns.TypeSRV)); len(ans) != 0 {
			t.Fatalf("SRV emitted with port 0: %v", ans)
		}
		if ans, _, _ := snap.Lookup(q("_caldavs._tcp.pat.example.com.", dns.TypeTXT)); len(ans) != 0 {
			t.Fatalf("TXT emitted with port 0: %v", ans)
		}
	})
}
