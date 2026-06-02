package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

const trsDDL = `CREATE TABLE IF NOT EXISTS tenant_runtime_state (
    tenant_id   TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 1,
    suspended   INTEGER NOT NULL DEFAULT 0,
    deny_status INTEGER NOT NULL DEFAULT 403,
    deny_reason TEXT    NOT NULL DEFAULT '',
    rate_limit_rps    INTEGER NOT NULL DEFAULT 0,
    rate_burst        INTEGER NOT NULL DEFAULT 0,
    concurrency_limit INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL DEFAULT ''
);`

func mustExecP(t *testing.T, pu *Unit, q string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(q); err != nil {
		t.Fatalf("exec: %v\n%s", err, q)
	}
}

// admissionUnit builds a test Unit with the tenant_runtime_state table and
// a single 'acme' tenant. Caller seeds the row (if any) and attaches the
// provider.
func admissionUnit(t *testing.T) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	mustExecP(t, pu, trsDDL)
	mustExecP(t, pu, `INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme','acme','acme','')`)
	return pu
}

func attachProvider(t *testing.T, pu *Unit) {
	t.Helper()
	prov := admission.NewSQLiteProvider(zap.NewNop())
	if err := prov.Rebuild(pu.Dbc.Db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	pu.Admission = prov
}

// TestAdmissionDeniesSuspendedTenantAtHandoff: a suspended tenant is denied
// with its deny_status (402) at the _sys->concrete handoff; the customer
// stack never runs (stop=true), and the denial is attributed to the tenant.
func TestAdmissionDeniesSuspendedTenantAtHandoff(t *testing.T) {
	pu := admissionUnit(t)
	mustExecP(t, pu, `INSERT INTO tenant_runtime_state (tenant_id, suspended, deny_status, deny_reason)
	                  VALUES ('tnt_acme', 1, 402, 'payment_required')`)
	attachProvider(t, pu)

	ctx := WithTenant(context.Background(), tenants.SystemTenantSlug)
	resp := `{"_txc":{"tenant":"acme","goto":"acme/web/0","web":{"req":{"method":"GET"}}}}`
	resCh := make(chan event.Payload, 1)
	opsDone := false
	stop, err := pu.advanceAfterScope(ctx, "boot/100", resp, nil, "", nil, &opsDone, resCh, func() {})
	if err != nil {
		t.Fatalf("advanceAfterScope: %v", err)
	}
	if !stop {
		t.Fatal("admission deny must stop (stop=true), skipping the customer stack")
	}
	if !opsDone {
		t.Fatal("admission deny must mark opsDone")
	}
	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got != 402 {
			t.Errorf("status = %d, want 402", got)
		}
		if got := gjson.Get(p.Raw, "_txc.tenant").String(); got != "acme" {
			t.Errorf("tenant = %q, want acme (usage attribution)", got)
		}
		if !gjson.Get(p.Raw, "_txc.admission.denied").Bool() {
			t.Error("admission.denied marker missing")
		}
		if got := gjson.Get(p.Raw, "_txc.web.res.headers.x-txc-deny-reason.0").String(); got != "payment_required" {
			t.Errorf("deny-reason = %q, want payment_required", got)
		}
	default:
		t.Fatal("expected a deny response on resCh")
	}
}

// TestAdmissionAllowsTenantWithoutRow: a tenant with no row is admitted;
// the handoff proceeds and no deny is emitted.
func TestAdmissionAllowsTenantWithoutRow(t *testing.T) {
	pu := admissionUnit(t) // acme has no tenant_runtime_state row
	attachProvider(t, pu)
	assertNoAdmissionDeny(t, pu, WithTenant(context.Background(), tenants.SystemTenantSlug))
}

// TestAdmissionSkipsAlreadyPinnedTenant: a request already pinned to a
// concrete tenant (resume / re-entry) is NOT re-gated even if that tenant
// is suspended — the hook fires only on the one-way _sys->concrete handoff.
func TestAdmissionSkipsAlreadyPinnedTenant(t *testing.T) {
	pu := admissionUnit(t)
	mustExecP(t, pu, `INSERT INTO tenant_runtime_state (tenant_id, suspended, deny_status) VALUES ('tnt_acme', 1, 402)`)
	attachProvider(t, pu)
	// Pin is already 'acme': maybeRetenant is a no-op, the pin is
	// unchanged, so the admission hook must not fire.
	assertNoAdmissionDeny(t, pu, WithTenant(context.Background(), "acme"))
}

// assertNoAdmissionDeny drives the handoff and fails if any payload on the
// channel is an admission deny. The hook runs synchronously inside
// advanceAfterScope, so a buggy fire lands in the buffer before it returns.
func assertNoAdmissionDeny(t *testing.T, pu *Unit, ctx context.Context) {
	t.Helper()
	resp := `{"_txc":{"tenant":"acme","goto":"acme/web/0","web":{"req":{"method":"GET"}}}}`
	resCh := make(chan event.Payload, 16)
	opsDone := false
	if _, err := pu.advanceAfterScope(ctx, "boot/100", resp, nil, "", nil, &opsDone, resCh, func() {}); err != nil {
		t.Fatalf("advanceAfterScope: %v", err)
	}
	for {
		select {
		case p := <-resCh:
			if gjson.Get(p.Raw, "_txc.admission.denied").Bool() || gjson.Get(p.Raw, "_txc.web.res.status").Int() == 402 {
				t.Fatalf("admitted/ungated tenant must not get an admission deny: %s", p.Raw)
			}
		default:
			return
		}
	}
}
