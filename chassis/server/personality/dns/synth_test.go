package dns

import (
	"database/sql"
	"testing"

	"github.com/miekg/dns"
)

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
