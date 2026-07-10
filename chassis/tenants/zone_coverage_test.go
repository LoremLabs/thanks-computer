package tenants

import (
	"context"
	"testing"
)

// acme owns example.com; beta owns the more-specific sub.example.com.
func seedZoneCoverage(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, db := newDNSStore(t)
	for _, tn := range []struct{ id, slug string }{{"t_a", "acme"}, {"t_b", "beta"}} {
		if _, err := db.Exec(`INSERT INTO tenants(tenant_id,slug,name,created_at) VALUES(?,?,?,'t')`,
			tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	mustZone(t, db, s, DNSZone{ID: NewZoneID(), TenantID: "t_a", Origin: "example.com",
		MName: "ns1.txco.io", RName: "h.example.com"})
	mustZone(t, db, s, DNSZone{ID: NewZoneID(), TenantID: "t_b", Origin: "sub.example.com",
		MName: "ns1.txco.io", RName: "h.sub.example.com"})
	return s, context.Background()
}

func TestDomainCoveredByZone(t *testing.T) {
	s, ctx := seedZoneCoverage(t)
	cases := []struct {
		slug, domain string
		want         bool
	}{
		{"acme", "example.com", true},       // apex
		{"acme", "mail.example.com", true},  // subdomain of acme's zone
		{"beta", "sub.example.com", true},   // apex of beta's zone
		{"beta", "a.sub.example.com", true}, // subdomain of beta's zone
		{"acme", "evilexample.com", false},  // not a dot-boundary subdomain
		{"acme", "other.com", false},        // unrelated
		{"beta", "example.com", false},      // beta doesn't own example.com
		{"acme", "", false},                 // empty
	}
	for _, c := range cases {
		got, err := DomainCoveredByZone(ctx, s.DB, c.slug, c.domain, nil)
		if err != nil || got != c.want {
			t.Errorf("DomainCoveredByZone(%q,%q)=%v,%v want %v", c.slug, c.domain, got, err, c.want)
		}
	}
}

func TestTenantForMailZoneLongestMatch(t *testing.T) {
	s, ctx := seedZoneCoverage(t)
	cases := []struct {
		domain   string
		wantSlug string
		wantOK   bool
	}{
		{"example.com", "acme", true},       // apex → acme
		{"mail.example.com", "acme", true},  // only acme's zone matches
		{"sub.example.com", "beta", true},   // matches both; longest (sub) → beta
		{"x.sub.example.com", "beta", true}, // most-specific zone wins
		{"nope.org", "", false},             // no zone
	}
	for _, c := range cases {
		slug, ok, err := TenantForMailZone(ctx, s.DB, c.domain, nil)
		if err != nil || ok != c.wantOK || slug != c.wantSlug {
			t.Errorf("TenantForMailZone(%q)=%q,%v,%v want %q,%v", c.domain, slug, ok, err, c.wantSlug, c.wantOK)
		}
	}
}

func TestTenantForMailZoneSkipsRevoked(t *testing.T) {
	s, ctx := seedZoneCoverage(t)
	// Revoke beta's sub.example.com → x.sub.example.com now falls back to acme.
	if _, err := s.DB.Exec(`UPDATE dns_zones SET revoked_at='t' WHERE origin='sub.example.com'`); err != nil {
		t.Fatal(err)
	}
	slug, ok, err := TenantForMailZone(ctx, s.DB, "x.sub.example.com", nil)
	if err != nil || !ok || slug != "acme" {
		t.Fatalf("after revoke want acme; got %q,%v,%v", slug, ok, err)
	}
	if cov, _ := DomainCoveredByZone(ctx, s.DB, "beta", "sub.example.com", nil); cov {
		t.Fatal("revoked zone must not count as covered")
	}
}
