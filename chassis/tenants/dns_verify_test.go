package tenants

import (
	"context"
	"testing"
)

func TestNSContainsAll(t *testing.T) {
	cases := []struct {
		name           string
		resolved, want []string
		ok             bool
	}{
		{"exact match", []string{"ns1.txco.io", "ns2.txco.io"}, []string{"ns1.txco.io", "ns2.txco.io"}, true},
		{"resolved has extras (still delegated to us)", []string{"ns1.txco.io", "ns2.txco.io", "x.other.net"}, []string{"ns1.txco.io", "ns2.txco.io"}, true},
		{"missing one of ours", []string{"ns1.txco.io"}, []string{"ns1.txco.io", "ns2.txco.io"}, false},
		{"trailing dot + case insensitive", []string{"NS1.TXCO.IO.", "ns2.txco.io."}, []string{"ns1.txco.io", "ns2.txco.io"}, true},
		{"delegated to a squatter's NS", []string{"ns1.squatter.com"}, []string{"ns1.txco.io"}, false},
		{"empty want", []string{"ns1.txco.io"}, nil, false},
		{"empty resolved", nil, []string{"ns1.txco.io"}, false},
	}
	for _, c := range cases {
		if got := nsContainsAll(c.resolved, c.want); got != c.ok {
			t.Errorf("%s: nsContainsAll(%v,%v)=%v want %v", c.name, c.resolved, c.want, got, c.ok)
		}
	}
}

// TestZoneVerificationGate: a zone with no verified_at (pending — created with
// --dns-require-zone-verification on) confers no authority; verifying it flips
// it live. Representative of the gate added to all dns_zones authority readers.
func TestZoneVerificationGate(t *testing.T) {
	s, db := newDNSStore(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO tenants(tenant_id,slug,name,created_at) VALUES('t_a','acme','acme','t')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	// Create a PENDING zone (VerifiedAt empty → verified_at NULL).
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateZoneTx(ctx, tx, DNSZone{ID: NewZoneID(), TenantID: "t_a",
		Origin: "pending.example", MName: "ns1.txco.io", RName: "h.pending.example"}); err != nil {
		t.Fatalf("CreateZoneTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if cov, err := DomainCoveredByZone(ctx, s.DB, "acme", "pending.example"); err != nil || cov {
		t.Fatalf("pending zone must NOT be a covered sender: cov=%v err=%v", cov, err)
	}
	if slug, ok, err := TenantForMailZone(ctx, s.DB, "pending.example"); err != nil || ok {
		t.Fatalf("pending zone must NOT route mail: slug=%q ok=%v err=%v", slug, ok, err)
	}

	// Verify it → now live.
	vtx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetZoneVerifiedTx(ctx, vtx, "t_a", "pending.example", "2026-06-20T00:00:00Z"); err != nil {
		t.Fatalf("SetZoneVerifiedTx: %v", err)
	}
	if err := vtx.Commit(); err != nil {
		t.Fatal(err)
	}

	if cov, err := DomainCoveredByZone(ctx, s.DB, "acme", "pending.example"); err != nil || !cov {
		t.Fatalf("verified zone must be a covered sender: cov=%v err=%v", cov, err)
	}
	if _, ok, err := TenantForMailZone(ctx, s.DB, "pending.example"); err != nil || !ok {
		t.Fatalf("verified zone must route mail: ok=%v err=%v", ok, err)
	}
}
