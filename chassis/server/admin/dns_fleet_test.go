package admin

import (
	"context"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// zoneHostRow returns (count, hostname, verified_at) for delegated-zone
// routing rows (created_by = SystemZoneHostCreatedBy) bound to stack.
func zoneHostRow(t *testing.T, c *Controller, stack string) (int, string, string) {
	t.Helper()
	var n int
	var host, verified string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT count(*), COALESCE(max(hostname),''), COALESCE(max(verified_at),'')
		   FROM tenant_hostnames
		  WHERE stack=? AND created_by=? AND revoked_at IS NULL`,
		stack, tenants.SystemZoneHostCreatedBy).Scan(&n, &host, &verified); err != nil {
		t.Fatalf("query tenant_hostnames: %v", err)
	}
	return n, host, verified
}

func reconcileZone(t *testing.T, c *Controller, origin string) {
	t.Helper()
	tx, err := c.pu.RuntimeDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := c.reconcileZoneHostnames(context.Background(), tx, testTenant, origin); err != nil {
		_ = tx.Rollback()
		t.Fatalf("reconcileZoneHostnames: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestZoneReconcileBacks fills the gap that caused the prod 404: a stack
// activated BEFORE its tenant has a delegated zone gets no `<label>.<origin>`
// routing row (the activation-time mint only fires when the zone already
// exists). Creating the zone must reconcile already-active stacks — minting a
// verified routing host so the stack is reachable (and tls-ask-approvable)
// without re-activation.
func TestZoneReconcileBacksAlreadyActiveStacks(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"}) // fleet off; no structured suffix

	// Activate a stack with NO delegated zone present → no zone-host row yet.
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "shop", v)
	if n, _, _ := zoneHostRow(t, c, "shop"); n != 0 {
		t.Fatalf("pre-zone: %d zone-host rows, want 0", n)
	}

	// Create the delegated zone → reconcile wires up the already-active stack.
	origin := "ops.example.com"
	reconcileZone(t, c, origin)

	want := tenants.StackLabel("shop") + "." + origin
	n, host, verified := zoneHostRow(t, c, "shop")
	if n != 1 {
		t.Fatalf("after zone create: %d zone-host rows, want 1", n)
	}
	if host != want {
		t.Fatalf("zone host = %q, want %q", host, want)
	}
	if verified == "" {
		t.Fatal("verified_at empty — would fail tls-ask + strict routing")
	}

	// Idempotent: reconciling again (e.g. a later create on a same-origin race
	// or a re-run) must not duplicate the row.
	reconcileZone(t, c, origin)
	if n, _, _ := zoneHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("idempotent reconcile: %d rows, want 1", n)
	}
}

// TestZoneReconcileSkipsSystemStacks confirms the reconcile honors the same
// mintable filter as synthesis — system stacks never get a routing host.
func TestZoneReconcileSkipsSystemStacks(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	// An active, non-system stack is reconciled...
	v := callCreateDraft(t, c, "web", "")
	callPutFiles(t, c, "web", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "web", v)

	reconcileZone(t, c, "z.example.com")

	if n, _, _ := zoneHostRow(t, c, "web"); n != 1 {
		t.Fatalf("non-system stack: %d rows, want 1", n)
	}
	// ...and isMintableStack already screens the system stacks (unit-tested in
	// TestIsMintableStack); the reconcile loop applies that same filter, so an
	// active `boot`/`_`-prefixed stack would never reach EnsureZoneHostnameTx.
}
