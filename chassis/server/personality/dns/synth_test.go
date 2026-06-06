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
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, mode, created_at, created_by, updated_at)
		VALUES ('dz_pat', ?, ?, 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, 'pattern', ?, 'seed', ?)`,
		tenantID, origin, ts, ts)
	if err != nil {
		t.Fatalf("insert pattern zone: %v", err)
	}
}

// seedActiveStack inserts an active stack (active_version → a
// stack_versions row carrying activated_at) for a tenant.
func seedActiveStack(t *testing.T, db *sql.DB, tenantID, name, activatedAt string) {
	t.Helper()
	sid := "stk_" + name
	if _, err := db.Exec(`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		VALUES (?, ?, ?, 1, ?)`, sid, tenantID, name, activatedAt); err != nil {
		t.Fatalf("insert stack: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO stack_versions
		(stack_id, version_number, status, created_by, created_at, activated_at)
		VALUES (?, 1, 'draft', 'seed', ?, ?)`, sid, activatedAt, activatedAt); err != nil {
		t.Fatalf("insert stack_version: %v", err)
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
		(id, tenant_id, origin, mname, rname, refresh, retry, expire, minimum, default_ttl, mode, created_at, created_by, updated_at)
		VALUES ('dz_man', ?, 'man.example.com', 'ns1.txco.io', 'hostmaster.txco.io', 7200, 3600, 1209600, 300, 300, 'manual', ?, 'seed', ?)`,
		patTenant, fixedTS, fixedTS); err != nil {
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
