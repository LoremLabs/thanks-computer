package processor

import (
	"context"
	"testing"
)

// TestOpsForStageTenantIsolation is the security regression for
// cross-tenant stack jumps. A txcl rule in tenant A's stack doing
// `EXEC "<B's stack>/0"` resolves a stage by bare stack name; the op
// lookup must scope that resolution to the request's tenant so A can
// never reach into B's stacks — and two tenants may independently own a
// stack of the same name without bleeding into each other.
func TestOpsForStageTenantIsolation(t *testing.T) {
	pu, _ := newTestUnit(t)

	for _, tn := range []struct{ id, slug string }{
		{"t-alpha", "alpha"},
		{"t-bravo", "bravo"},
	} {
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, '')`, tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("seed tenant %s: %v", tn.slug, err)
		}
	}

	seed := func(tenantID, stack string, scope int, txcl string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', ?, '', '')`,
			tenantID, stack, scope, txcl); err != nil {
			t.Fatalf("seed (%s,%s,%d): %v", tenantID, stack, scope, err)
		}
	}

	// Tenant B owns "secret"; tenant A does not.
	seed("t-bravo", "secret", 0, `EXEC "txco://bravo-secret"`)
	// Both tenants independently own a stack named "shared".
	seed("t-alpha", "shared", 0, `EXEC "txco://alpha-shared"`)
	seed("t-bravo", "shared", 0, `EXEC "txco://bravo-shared"`)

	t.Run("cross-tenant jump is blocked", func(t *testing.T) {
		// Tenant A's rule did EXEC "secret/0". The stage resolves by
		// bare name; scoped to A, "secret" must not be found.
		ops, err := pu.OpsForStage(WithTenant(context.Background(), "alpha"), "secret/0")
		if err != nil {
			t.Fatalf("OpsForStage: %v", err)
		}
		if len(ops) != 0 {
			t.Fatalf("tenant A reached %d ops in tenant B's 'secret' stack, want 0: %+v", len(ops), ops)
		}
	})

	t.Run("owner can resolve its own stack", func(t *testing.T) {
		ops, err := pu.OpsForStage(WithTenant(context.Background(), "bravo"), "secret/0")
		if err != nil {
			t.Fatalf("OpsForStage: %v", err)
		}
		if len(ops) != 1 {
			t.Fatalf("tenant B got %d ops for its own 'secret', want 1: %+v", len(ops), ops)
		}
	})

	t.Run("same stack name is isolated per tenant", func(t *testing.T) {
		aOps, err := pu.OpsForStage(WithTenant(context.Background(), "alpha"), "shared/0")
		if err != nil {
			t.Fatalf("OpsForStage alpha: %v", err)
		}
		if len(aOps) != 1 || aOps[0].Txcl != `EXEC "txco://alpha-shared"` {
			t.Fatalf("tenant A 'shared' resolved to %+v, want only alpha-shared", aOps)
		}
		bOps, err := pu.OpsForStage(WithTenant(context.Background(), "bravo"), "shared/0")
		if err != nil {
			t.Fatalf("OpsForStage bravo: %v", err)
		}
		if len(bOps) != 1 || bOps[0].Txcl != `EXEC "txco://bravo-shared"` {
			t.Fatalf("tenant B 'shared' resolved to %+v, want only bravo-shared", bOps)
		}
	})

	t.Run("empty pin matches only the NULL-tenant bucket, never tenant rows", func(t *testing.T) {
		// There is no global/unfiltered path anymore. An empty pin
		// resolves ONLY tenant_id IS NULL rows (legacy/test data);
		// it must never reach a real tenant's ops.
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (NULL, ?, ?, '', ?, '', '')`,
			"legacy", 0, `EXEC "txco://legacy"`); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
		legacy, err := pu.OpsForStage(context.Background(), "legacy/0")
		if err != nil {
			t.Fatalf("OpsForStage legacy: %v", err)
		}
		if len(legacy) != 1 {
			t.Fatalf("empty pin got %d ops for NULL-tenant 'legacy', want 1: %+v", len(legacy), legacy)
		}
		// And the empty pin must NOT reach tenant B's 'secret'.
		leaked, err := pu.OpsForStage(context.Background(), "secret/0")
		if err != nil {
			t.Fatalf("OpsForStage secret: %v", err)
		}
		if len(leaked) != 0 {
			t.Fatalf("empty pin reached %d tenant-owned ops, want 0: %+v", len(leaked), leaked)
		}
	})
}

// TestBootRetenantGate covers the one-way _sys -> concrete-tenant
// transition: the only place a request's pinned tenant may change
// after first Run. It must fire only from the system context, only to
// an existing non-revoked tenant, and never from a concrete tenant.
func TestBootRetenantGate(t *testing.T) {
	pu, _ := newTestUnit(t)

	for _, tn := range []struct{ id, slug, revoked string }{
		{"tnt_sys", "_sys", ""},
		{"tnt_acme", "acme", ""},
		{"tnt_gone", "gone", "2026-01-01T00:00:00Z"},
	} {
		var rev any
		if tn.revoked != "" {
			rev = tn.revoked
		}
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO tenants (tenant_id, slug, name, created_at, revoked_at) VALUES (?, ?, ?, '', ?)`,
			tn.id, tn.slug, tn.slug, rev); err != nil {
			t.Fatalf("seed tenant %s: %v", tn.slug, err)
		}
	}

	const sys = "_sys"
	cases := []struct {
		name     string
		pinned   string
		respTen  string
		wantPin  string
	}{
		{"sys -> acme (valid handoff)", sys, "acme", "acme"},
		{"sys -> _sys (no-op)", sys, "_sys", sys},
		{"sys -> unknown (rejected, stays sys)", sys, "nope", sys},
		{"sys -> revoked (rejected, stays sys)", sys, "gone", sys},
		{"sys -> empty (no request, stays sys)", sys, "", sys},
		{"concrete pin is immutable (acme -> evil)", "acme", "evil", "acme"},
		{"concrete cannot drop back to _sys", "acme", "_sys", "acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithTenant(context.Background(), tc.pinned)
			resp := `{}`
			if tc.respTen != "" {
				resp = `{"_txc":{"tenant":"` + tc.respTen + `"}}`
			}
			got := tenantScope(pu.maybeRetenant(ctx, resp))
			if got != tc.wantPin {
				t.Fatalf("pin after maybeRetenant = %q, want %q", got, tc.wantPin)
			}
		})
	}
}

// TestBootRetenantComposesWithLookup ties the gate to the op lookup:
// from the _sys context a boot rule's re-tenant must make the target
// tenant's stacks resolvable while _sys's own boot ops fall out of
// scope — proving the handoff actually re-scopes resolution.
func TestBootRetenantComposesWithLookup(t *testing.T) {
	pu, _ := newTestUnit(t)

	for _, tn := range []struct{ id, slug string }{
		{"tnt_sys", "_sys"},
		{"tnt_acme", "acme"},
	} {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, '')`,
			tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("seed tenant %s: %v", tn.slug, err)
		}
	}
	seed := func(tenantID, stack string, scope int, txcl string) {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', ?, '', '')`,
			tenantID, stack, scope, txcl); err != nil {
			t.Fatalf("seed op: %v", err)
		}
	}
	seed("tnt_sys", "boot/router", 0, `EXEC "txco://sys-router"`)
	seed("tnt_acme", "acme/web", 0, `EXEC "txco://acme-web"`)

	// In the boot/_sys context, acme's stack is invisible...
	sysCtx := WithTenant(context.Background(), "_sys")
	if ops, _ := pu.OpsForStage(sysCtx, "acme/web/0"); len(ops) != 0 {
		t.Fatalf("_sys context resolved acme/web, want 0: %+v", ops)
	}
	if ops, _ := pu.OpsForStage(sysCtx, "boot/router/0"); len(ops) != 1 {
		t.Fatalf("_sys context should see its own boot/router, got %d", len(ops))
	}

	// ...until a boot rule re-tenants to acme, after which acme's
	// stack resolves and _sys's boot ops no longer do.
	acmeCtx := pu.maybeRetenant(sysCtx, `{"_txc":{"tenant":"acme"}}`)
	if ops, _ := pu.OpsForStage(acmeCtx, "acme/web/0"); len(ops) != 1 {
		t.Fatalf("after re-tenant, acme/web should resolve, got %d", len(ops))
	}
	if ops, _ := pu.OpsForStage(acmeCtx, "boot/router/0"); len(ops) != 0 {
		t.Fatalf("after re-tenant, _sys boot/router must be out of scope, got %d: %+v", len(ops), ops)
	}
}
