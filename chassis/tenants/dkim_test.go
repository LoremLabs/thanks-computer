package tenants

import (
	"context"
	"testing"
)

func TestEnsureSystemHostnameStoresDKIM(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	mustCreateTenant(t, s, "tnt_a", "alpha")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := EnsureSystemHostnameTx(ctx, tx, "tnt_a", "web", ".stacks.example.com", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("EnsureSystemHostnameTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var sel, priv, pub string
	if err := db.QueryRow(
		`SELECT dkim_selector, dkim_private_pem, dkim_public_b64 FROM tenant_hostnames WHERE hostname = ?`,
		host).Scan(&sel, &priv, &pub); err != nil {
		t.Fatalf("read minted host: %v", err)
	}
	if sel != DKIMSelector || priv == "" || pub == "" {
		t.Fatalf("structured host missing per-host key: sel=%q privLen=%d pubLen=%d", sel, len(priv), len(pub))
	}

	// Idempotent re-mint returns the same host (no second key).
	tx2, _ := db.BeginTx(ctx, nil)
	host2, err := EnsureSystemHostnameTx(ctx, tx2, "tnt_a", "web", ".stacks.example.com", "2026-01-02T00:00:00Z")
	_ = tx2.Commit()
	if err != nil || host2 != host {
		t.Fatalf("re-mint not idempotent: %q vs %q (%v)", host2, host, err)
	}
}

func TestDKIMSignerForDomain(t *testing.T) {
	s, db := newDNSStore(t)
	ctx := context.Background()

	// A structured host with its own per-host key.
	if _, err := db.Exec(`INSERT INTO tenant_hostnames
		(id, hostname, tenant_id, stack, created_at, created_by, verified_at, dkim_selector, dkim_private_pem)
		VALUES ('h_x','web-x.stacks.example.com','tnt_a','web','t','system:structured-host','t','txco','PRIVHOST')`); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	// A delegated zone (CreateZoneTx mints its own key).
	mustZone(t, db, s, DNSZone{ID: NewZoneID(), TenantID: "tnt_a", Origin: "acme.com",
		MName: "ns1.txco.io", RName: "h.acme.com"})

	t.Run("exact structured host → d=host, per-host key", func(t *testing.T) {
		sdid, sel, priv, ok, err := DKIMSignerForDomain(ctx, db, "web-x.stacks.example.com")
		if err != nil || !ok || sdid != "web-x.stacks.example.com" || sel != "txco" || priv != "PRIVHOST" {
			t.Fatalf("got %q/%q/%q ok=%v err=%v", sdid, sel, priv, ok, err)
		}
	})
	t.Run("delegated zone (subdomain) → d=zone", func(t *testing.T) {
		sdid, _, priv, ok, err := DKIMSignerForDomain(ctx, db, "mail.acme.com")
		if err != nil || !ok || sdid != "acme.com" || priv == "" {
			t.Fatalf("got %q priv?%v ok=%v err=%v", sdid, priv != "", ok, err)
		}
	})
	t.Run("no signer", func(t *testing.T) {
		if _, _, _, ok, err := DKIMSignerForDomain(ctx, db, "nope.org"); ok || err != nil {
			t.Fatalf("want no signer; ok=%v err=%v", ok, err)
		}
	})
}
