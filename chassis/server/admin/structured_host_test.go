package admin

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// TestBackfillStructuredHostDKIM: a structured host minted before the 0017 key
// columns (simulated by clearing its key) gets one from the backfill, and a
// second run is a no-op.
func TestBackfillStructuredHostDKIM(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "shop", v)
	_, host := systemHostRow(t, c, "shop")

	// Simulate a pre-0017 (keyless) structured host.
	if _, err := c.pu.RuntimeDB.Exec(
		`UPDATE tenant_hostnames SET dkim_selector='', dkim_private_pem='', dkim_public_b64='' WHERE hostname = ?`,
		host); err != nil {
		t.Fatalf("clear key: %v", err)
	}

	n, err := c.BackfillStructuredHostDKIM(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("backfill: n=%d err=%v want 1", n, err)
	}
	var sel, priv, pub string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT dkim_selector, dkim_private_pem, dkim_public_b64 FROM tenant_hostnames WHERE hostname = ?`,
		host).Scan(&sel, &priv, &pub); err != nil {
		t.Fatalf("read host: %v", err)
	}
	if sel != tenants.DKIMSelector || priv == "" || pub == "" {
		t.Fatalf("host not keyed: sel=%q privLen=%d pubLen=%d", sel, len(priv), len(pub))
	}

	// Idempotent: nothing left to key.
	if n2, err := c.BackfillStructuredHostDKIM(context.Background()); err != nil || n2 != 0 {
		t.Fatalf("second backfill: n=%d err=%v want 0", n2, err)
	}
}

func TestStructuredURLSchemeDerivation(t *testing.T) {
	host := "shop-ab2cd3.localhost"
	prodHost := "shop-ab2cd3.stacks.thanks.computer"

	// Plain HTTP dev request → http + web port appended.
	r := httptest.NewRequest("POST", "http://admin.local:8081/x", nil)
	if got := structuredURL(r, host, ":8080"); got != "http://"+host+":8080" {
		t.Fatalf("dev: got %q, want http://%s:8080", got, host)
	}

	// Behind Caddy: X-Forwarded-Proto=https → https, no port.
	r2 := httptest.NewRequest("POST", "http://admin/x", nil)
	r2.Header.Set("X-Forwarded-Proto", "https")
	if got := structuredURL(r2, prodHost, ":8080"); got != "https://"+prodHost {
		t.Fatalf("prod: got %q, want https://%s (no port)", got, prodHost)
	}

	// http on standard port 80 → no port suffix.
	if got := structuredURL(r, host, ":80"); got != "http://"+host {
		t.Fatalf("port-80: got %q, want http://%s", got, host)
	}
}

// systemHostRow returns (count, anyHostname) for chassis-minted rows
// bound to stack across all tenants in the test DB.
func systemHostRow(t *testing.T, c *Controller, stack string) (int, string) {
	t.Helper()
	var n int
	var host string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT count(*), COALESCE(max(hostname),'')
		   FROM tenant_hostnames
		  WHERE stack=? AND created_by=? AND revoked_at IS NULL`,
		stack, tenants.SystemStructuredHostCreatedBy).Scan(&n, &host); err != nil {
		t.Fatalf("query tenant_hostnames: %v", err)
	}
	return n, host
}

func TestActivateMintsStructuredHostname(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})

	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "shop", v)

	n, host := systemHostRow(t, c, "shop")
	if n != 1 {
		t.Fatalf("after activate: %d system hostname rows, want 1", n)
	}
	if want := "shop-"; host[:len(want)] != want {
		t.Fatalf("hostname %q: want prefix %q", host, want)
	}
	if host[len(host)-len(".localhost"):] != ".localhost" {
		t.Fatalf("hostname %q: want .localhost suffix", host)
	}
	var verifiedAt, createdBy string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT COALESCE(verified_at,''), created_by FROM tenant_hostnames WHERE hostname=?`,
		host).Scan(&verifiedAt, &createdBy); err != nil {
		t.Fatalf("row read: %v", err)
	}
	if verifiedAt == "" {
		t.Fatal("verified_at empty — would NOT route under strict resolver")
	}
	if createdBy != tenants.SystemStructuredHostCreatedBy {
		t.Fatalf("created_by=%q, want sentinel", createdBy)
	}

	// Re-activate a fresh version → same hostname reused, no dup row.
	v2 := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v2, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/z"`},
	})
	callActivate(t, c, "shop", v2)
	n2, host2 := systemHostRow(t, c, "shop")
	if n2 != 1 || host2 != host {
		t.Fatalf("re-activate: count=%d host=%q; want idempotent reuse of %q", n2, host2, host)
	}
}

// TestActivatePublishesStructuredHostname: with the fleet producer enabled,
// activating a stack must enqueue a hostname.bound control event for the
// auto-minted structured host. Without it the host stays on the control-plane
// node and every data-plane node 404s the stack's URL (the bug this fixes).
func TestActivatePublishesStructuredHostname(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
		FeedSink:             "file",
	})
	withAStore(t, c) // fleetEnabled() also needs an artifact store
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "shop", v)

	// The structured host was minted on the control-plane path...
	if n, host := systemHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("after activate: %d structured host rows (%q), want 1", n, host)
	}

	// ...and a hostname.bound event was queued so the fleet learns it.
	var bound int
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT count(*) FROM control_events_outbox WHERE event_type = ? AND tenant_id = ?`,
		controlevent.TypeHostnameBound, testTenant).Scan(&bound); err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	if bound != 1 {
		t.Fatalf("hostname.bound outbox rows = %d, want 1 (structured host not propagated to the fleet)", bound)
	}
}

// TestApplyStackVersionDoesNotMint: the data-plane applier path
// (ApplyStackVersion → materialiseStackVersion with mintHosts=false) must NOT
// mint its own structured host. MintHandle is random, so a per-node mint would
// diverge and the stack's URL would 404 on every node but the one that minted
// it; the canonical host arrives via a hostname.bound event instead. The suffix
// is configured here precisely to prove the data-plane path still skips minting.
func TestApplyStackVersionDoesNotMint(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})

	ctx := context.Background()
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := c.ApplyStackVersion(ctx, tx, testTenant, "shop", v, "2026-06-08T00:00:00.000Z"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ApplyStackVersion: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n, host := systemHostRow(t, c, "shop"); n != 0 {
		t.Fatalf("data-plane apply minted %d host row(s) (%q); want 0", n, host)
	}
}

func TestActivateNoMintWhenSuffixEmpty(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"}) // no suffix
	v := callCreateDraft(t, c, "plain", "")
	callPutFiles(t, c, "plain", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "plain", v)
	if n, _ := systemHostRow(t, c, "plain"); n != 0 {
		t.Fatalf("suffix empty: %d minted rows, want 0 (feature off)", n)
	}
}

func TestIsMintableStack(t *testing.T) {
	mintable := []string{"shop", "web", "website/canary", "a"}
	skipped := []string{"", "_sys", "_cron", "boot", "boot/0", "BOOT/1", "txc-continuation",
		// nested `_`-prefixed convention handlers — a stack's mail/cron
		// entry (test-01/_mail), not a web app, so no structured hostname.
		"test-01/_mail", "test-01/_cron", "website/canary/_mail"}
	for _, s := range mintable {
		if !isMintableStack(s) {
			t.Errorf("isMintableStack(%q)=false, want true", s)
		}
	}
	for _, s := range skipped {
		if isMintableStack(s) {
			t.Errorf("isMintableStack(%q)=true, want false (system stack)", s)
		}
	}
}
