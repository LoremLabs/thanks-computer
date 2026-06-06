package tenants

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// newDNSStore extends newTestStore with the dns_zones/dns_records
// tables (0011 + 0012, inline) so the zone/record CRUD + activation
// helpers can be exercised without filesystem migrations.
func newDNSStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	s, db := newTestStore(t)
	if _, err := db.Exec(`
		CREATE TABLE dns_zones (
			id          TEXT PRIMARY KEY,
			tenant_id   TEXT NOT NULL,
			origin      TEXT NOT NULL,
			mname       TEXT NOT NULL,
			rname       TEXT NOT NULL,
			refresh     INTEGER NOT NULL DEFAULT 7200,
			retry       INTEGER NOT NULL DEFAULT 3600,
			expire      INTEGER NOT NULL DEFAULT 1209600,
			minimum     INTEGER NOT NULL DEFAULT 300,
			default_ttl INTEGER NOT NULL DEFAULT 300,
			mode        TEXT NOT NULL DEFAULT 'pattern' CHECK (mode IN ('pattern','manual')),
			created_at  TEXT NOT NULL,
			created_by  TEXT,
			updated_at  TEXT NOT NULL,
			revoked_at  TEXT,
			dkim_selector    TEXT NOT NULL DEFAULT '',
			dkim_private_pem TEXT NOT NULL DEFAULT '',
			dkim_public_b64  TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX dns_zones_active_origin_idx
		    ON dns_zones(origin) WHERE revoked_at IS NULL;
		CREATE TABLE dns_records (
			id         TEXT PRIMARY KEY,
			zone_id    TEXT NOT NULL,
			name       TEXT NOT NULL,
			type       TEXT NOT NULL CHECK (type IN ('NS','A','AAAA','MX','TXT')),
			ttl        INTEGER,
			rdata      TEXT NOT NULL,
			created_at TEXT NOT NULL,
			created_by TEXT,
			updated_at TEXT NOT NULL,
			revoked_at TEXT
		);
		CREATE INDEX dns_records_active_zone_idx
		    ON dns_records(zone_id) WHERE revoked_at IS NULL;
		CREATE TABLE dns_settings (
			singleton   INTEGER PRIMARY KEY CHECK (singleton = 1),
			nameservers TEXT NOT NULL DEFAULT '',
			edge_ips    TEXT NOT NULL DEFAULT '',
			mx_host     TEXT NOT NULL DEFAULT '',
			mx_priority INTEGER NOT NULL DEFAULT 10,
			synth_ttl   INTEGER NOT NULL DEFAULT 300,
			updated_at  TEXT NOT NULL,
			updated_by  TEXT
		);
	`); err != nil {
		t.Fatalf("create dns schema: %v", err)
	}
	return s, db
}

func mustZone(t *testing.T, db *sql.DB, s *Store, z DNSZone) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := s.CreateZoneTx(ctx, tx, z); err != nil {
		_ = tx.Rollback()
		t.Fatalf("CreateZoneTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestZoneCRUD(t *testing.T) {
	s, db := newDNSStore(t)
	ctx := context.Background()

	mustZone(t, db, s, DNSZone{
		ID: NewZoneID(), TenantID: "t1", Origin: "ops.example.com",
		MName: "ns1.txco.io", RName: "hostmaster.ops.example.com",
	})

	// SOA timers + mode defaulted.
	zones, err := s.ListZones(ctx, "t1", false)
	if err != nil || len(zones) != 1 {
		t.Fatalf("ListZones: %v n=%d", err, len(zones))
	}
	if zones[0].Mode != "pattern" || zones[0].Minimum != 90 || zones[0].DefaultTTL != 60 {
		t.Fatalf("defaults not applied: %+v", zones[0])
	}

	// Duplicate active origin → ErrZoneExists.
	tx, _ := db.BeginTx(ctx, nil)
	derr := s.CreateZoneTx(ctx, tx, DNSZone{
		ID: NewZoneID(), TenantID: "t2", Origin: "ops.example.com",
		MName: "ns1.txco.io", RName: "hostmaster.ops.example.com",
	})
	_ = tx.Rollback()
	if !errors.Is(derr, ErrZoneExists) {
		t.Fatalf("want ErrZoneExists, got %v", derr)
	}

	// Lookup, then revoke, then it's gone from the active list.
	if _, err := s.LookupActiveZone(ctx, "t1", "OPS.example.com."); err != nil {
		t.Fatalf("LookupActiveZone (canonicalization): %v", err)
	}
	rtx, _ := db.BeginTx(ctx, nil)
	if _, err := s.RevokeZoneTx(ctx, rtx, "t1", "ops.example.com"); err != nil {
		t.Fatalf("RevokeZoneTx: %v", err)
	}
	_ = rtx.Commit()
	if zs, _ := s.ListZones(ctx, "t1", false); len(zs) != 0 {
		t.Fatalf("zone still active after revoke: %d", len(zs))
	}
}

func TestRecordCRUD(t *testing.T) {
	s, db := newDNSStore(t)
	ctx := context.Background()
	zid := NewZoneID()
	mustZone(t, db, s, DNSZone{
		ID: zid, TenantID: "t1", Origin: "ops.example.com",
		MName: "ns1.txco.io", RName: "hostmaster.ops.example.com",
	})

	tx, _ := db.BeginTx(ctx, nil)
	if err := s.CreateRecordTx(ctx, tx, DNSRecord{
		ID: NewRecordID(), ZoneID: zid, Name: "@", Type: "txt", Rdata: "hello",
	}); err != nil {
		t.Fatalf("CreateRecordTx: %v", err)
	}
	_ = tx.Commit()

	recs, err := s.ListRecords(ctx, zid)
	if err != nil || len(recs) != 1 || recs[0].Type != "TXT" {
		t.Fatalf("ListRecords: %v %+v", err, recs)
	}

	// Invalid type rejected.
	btx, _ := db.BeginTx(ctx, nil)
	if err := s.CreateRecordTx(ctx, btx, DNSRecord{ID: NewRecordID(), ZoneID: zid, Name: "@", Type: "CNAME", Rdata: "x"}); err == nil {
		t.Fatal("expected invalid type error")
	}
	_ = btx.Rollback()

	rtx, _ := db.BeginTx(ctx, nil)
	if err := s.RevokeRecordTx(ctx, rtx, zid, "@", "TXT"); err != nil {
		t.Fatalf("RevokeRecordTx: %v", err)
	}
	_ = rtx.Commit()
	if rs, _ := s.ListRecords(ctx, zid); len(rs) != 0 {
		t.Fatalf("record still active: %d", len(rs))
	}
}

func TestDNSSettingsRoundTrip(t *testing.T) {
	_, db := newDNSStore(t)
	ctx := context.Background()

	// No row yet → found=false (callers fall back to flag defaults).
	if _, found, err := LoadDNSSettings(ctx, db); err != nil || found {
		t.Fatalf("want not-found, got found=%v err=%v", found, err)
	}

	tx, _ := db.BeginTx(ctx, nil)
	if err := PutDNSSettingsTx(ctx, tx, DNSSettings{
		Nameservers: []string{"ns1.txco.io", "ns2.txco.io"},
		EdgeIPs:     []string{"203.0.113.10"},
		MXHost:      "mx.txco.io", MXPriority: 10, SynthTTL: 300, UpdatedBy: "op",
	}); err != nil {
		t.Fatalf("PutDNSSettingsTx: %v", err)
	}
	_ = tx.Commit()

	got, found, err := LoadDNSSettings(ctx, db)
	if err != nil || !found {
		t.Fatalf("load after put: found=%v err=%v", found, err)
	}
	if len(got.Nameservers) != 2 || got.EdgeIPs[0] != "203.0.113.10" || got.MXHost != "mx.txco.io" || got.SynthTTL != 300 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Upsert (singleton) — change MX; must replace, not duplicate.
	tx2, _ := db.BeginTx(ctx, nil)
	if err := PutDNSSettingsTx(ctx, tx2, DNSSettings{
		Nameservers: got.Nameservers, EdgeIPs: got.EdgeIPs,
		MXHost: "mx2.txco.io", MXPriority: 20, SynthTTL: 600,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_ = tx2.Commit()
	got2, _, _ := LoadDNSSettings(ctx, db)
	if got2.MXHost != "mx2.txco.io" || got2.MXPriority != 20 || got2.SynthTTL != 600 {
		t.Fatalf("upsert not applied: %+v", got2)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM dns_settings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("singleton broken: n=%d err=%v", n, err)
	}
}

func TestActivationHelpers(t *testing.T) {
	s, db := newDNSStore(t)
	ctx := context.Background()
	mustZone(t, db, s, DNSZone{
		ID: NewZoneID(), TenantID: "t1", Origin: "ops.example.com",
		MName: "ns1.txco.io", RName: "hostmaster.ops.example.com", Mode: "pattern",
	})

	tx, _ := db.BeginTx(ctx, nil)
	defer func() { _ = tx.Commit() }()

	origin, ok, err := ActivePatternZoneOriginTx(ctx, tx, "t1")
	if err != nil || !ok || origin != "ops.example.com" {
		t.Fatalf("ActivePatternZoneOriginTx: %q ok=%v err=%v", origin, ok, err)
	}
	// A tenant with no zone → ok=false.
	if _, ok, _ := ActivePatternZoneOriginTx(ctx, tx, "nobody"); ok {
		t.Fatal("expected no zone for unknown tenant")
	}

	// Deterministic label, matches StackLabel — and pre-verified.
	host, err := EnsureZoneHostnameTx(ctx, tx, "t1", "web-api", origin, "2026-05-29T14:32:07Z")
	if err != nil || host != "web-api.ops.example.com" {
		t.Fatalf("EnsureZoneHostnameTx: %q err=%v", host, err)
	}
	// Idempotent.
	host2, _ := EnsureZoneHostnameTx(ctx, tx, "t1", "web-api", origin, "2026-05-29T14:32:07Z")
	if host2 != host {
		t.Fatalf("not idempotent: %q vs %q", host2, host)
	}
	var verifiedAt sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT verified_at FROM tenant_hostnames WHERE hostname = ? AND created_by = ?`,
		host, SystemZoneHostCreatedBy).Scan(&verifiedAt); err != nil {
		t.Fatalf("lookup minted host: %v", err)
	}
	if !verifiedAt.Valid {
		t.Fatal("zone hostname should be pre-verified")
	}
}
