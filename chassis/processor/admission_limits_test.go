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

const handoffResp = `{"_txc":{"tenant":"acme","goto":"acme/web/0","web":{"req":{"method":"GET"}}}}`

func sysCtx() context.Context {
	return WithTenant(context.Background(), tenants.SystemTenantSlug)
}

// limitsProvider seeds a limit row, builds a real provider, and attaches it —
// returning the provider so the test can pre-drain rate tokens / fill slots.
func limitsProvider(t *testing.T, pu *Unit, insert string) *admissionProviderHandle {
	t.Helper()
	mustExecP(t, pu, insert)
	prov := admission.NewSQLiteProvider(zap.NewNop())
	if err := prov.Rebuild(pu.Dbc.Db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	pu.Admission = prov
	return &admissionProviderHandle{prov}
}

type admissionProviderHandle struct{ p admission.Provider }

func TestAdmissionRateLimited(t *testing.T) {
	pu := admissionUnit(t)
	h := limitsProvider(t, pu, `INSERT INTO tenant_runtime_state (tenant_id, rate_limit_rps, rate_burst) VALUES ('tnt_acme', 1, 1)`)
	// Drain the single burst token so the gate's own check denies.
	if ok, _ := h.p.AllowRate("acme"); !ok {
		t.Fatal("first token should be available")
	}

	resCh := make(chan event.Payload, 1)
	opsDone := false
	stop, err := pu.advanceAfterScope(sysCtx(), "boot/100", handoffResp, nil, "", nil, &opsDone, resCh, func() {})
	if err != nil || !stop {
		t.Fatalf("want stop,nil; got stop=%v err=%v", stop, err)
	}
	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.admission.status").Int(); got != 429 {
			t.Errorf("status = %d, want 429", got)
		}
		if got := gjson.Get(p.Raw, "_txc.admission.reason").String(); got != "rate_limited" {
			t.Errorf("reason = %q, want rate_limited", got)
		}
		if got := gjson.Get(p.Raw, "_txc.admission.retry_after").Int(); got < 1 {
			t.Errorf("retry_after = %d, want >= 1", got)
		}
	default:
		t.Fatal("expected a 429 on resCh")
	}
}

func TestAdmissionAtCapacity(t *testing.T) {
	pu := admissionUnit(t)
	h := limitsProvider(t, pu, `INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_acme', 1)`)
	// Fill the only slot so the gate's acquire fails.
	if !h.p.AcquireConcurrency("acme", admission.NewLease()) {
		t.Fatal("first slot should be available")
	}

	resCh := make(chan event.Payload, 1)
	opsDone := false
	stop, err := pu.advanceAfterScope(sysCtx(), "boot/100", handoffResp, nil, "", nil, &opsDone, resCh, func() {})
	if err != nil || !stop {
		t.Fatalf("want stop,nil; got stop=%v err=%v", stop, err)
	}
	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.admission.status").Int(); got != 429 {
			t.Errorf("status = %d, want 429", got)
		}
		if got := gjson.Get(p.Raw, "_txc.admission.reason").String(); got != "at_capacity" {
			t.Errorf("reason = %q, want at_capacity", got)
		}
	default:
		t.Fatal("expected a 429 on resCh")
	}
}

// TestAdmissionConcurrencySlotReleased: a request admitted through the gate
// holds a slot until its lease releases (what the bus-loop defer does).
func TestAdmissionConcurrencySlotReleased(t *testing.T) {
	pu := admissionUnit(t)
	h := limitsProvider(t, pu, `INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_acme', 1)`)

	lease := admission.NewLease()
	ctx := admission.WithLease(sysCtx(), lease)
	resCh := make(chan event.Payload, 8)
	opsDone := false
	if _, err := pu.advanceAfterScope(ctx, "boot/100", handoffResp, nil, "", nil, &opsDone, resCh, func() {}); err != nil {
		t.Fatalf("advanceAfterScope: %v", err)
	}
	// The gate took the slot; a fresh acquire must fail while it's held.
	if h.p.AcquireConcurrency("acme", admission.NewLease()) {
		t.Fatal("slot should be held after the gate acquired it")
	}
	lease.Release() // bus-loop defer equivalent
	if !h.p.AcquireConcurrency("acme", admission.NewLease()) {
		t.Fatal("slot should be free after the lease released")
	}
}
