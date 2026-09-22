package processor

import (
	"context"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// Envelope.Accepted: the processor tells an inlet, once, that a request
// has been routed into a concrete tenant and admitted — before that
// tenant's stack runs. An outbox-style inlet closes its delivery claim
// on this signal rather than at completion. These tests drive the same
// _sys->tenant handoff the admission tests do (advanceAfterScope from
// boot/100) with an observer wired to a channel.

const acceptResp = `{"_txc":{"tenant":"acme","stack":"web","goto":"acme/web/0","web":{"req":{"method":"GET"}}}}`

// acceptCtx pins _sys with the observer already attached, so the pin
// (and the later re-pin) are recorded into it — the bus loop's order.
func acceptCtx(obs *TenantObserver) context.Context {
	return WithTenant(WithTenantObserver(context.Background(), obs), tenants.SystemTenantSlug)
}

func wiredObserver(buf int) (*TenantObserver, chan event.Acceptance) {
	obs := NewTenantObserver()
	ch := make(chan event.Acceptance, buf)
	obs.NotifyAccepted(ch)
	return obs, ch
}

func handoff(t *testing.T, pu *Unit, ctx context.Context, resp string) (stop bool) {
	t.Helper()
	resCh := make(chan event.Payload, 16)
	opsDone := false
	stop, err := pu.advanceAfterScope(ctx, "boot/100", resp, nil, "", nil, &opsDone, resCh, func() {})
	if err != nil {
		t.Fatalf("advanceAfterScope: %v", err)
	}
	return stop
}

// TestAcceptanceFiresAtAdmittedHandoff: an admitted re-tenant sends one
// acceptance carrying the pinned tenant and the routed stack, both read
// from the observer (immutable pipeline state), not the envelope.
func TestAcceptanceFiresAtAdmittedHandoff(t *testing.T) {
	pu := admissionUnit(t)
	attachProvider(t, pu) // acme has no runtime row: admitted
	obs, ch := wiredObserver(1)
	handoff(t, pu, acceptCtx(obs), acceptResp)
	select {
	case a := <-ch:
		if a.Tenant != "acme" || a.Stack != "web" {
			t.Fatalf("acceptance = %+v, want {acme web}", a)
		}
	default:
		t.Fatal("expected an acceptance at the admitted handoff")
	}
}

// TestAcceptanceFiresWithoutAdmissionProvider: open core runs with no
// admission provider; acceptance is gated on the pin changing, not on
// admission being configured.
func TestAcceptanceFiresWithoutAdmissionProvider(t *testing.T) {
	pu := admissionUnit(t) // pu.Admission stays nil
	obs, ch := wiredObserver(1)
	handoff(t, pu, acceptCtx(obs), acceptResp)
	if len(ch) != 1 {
		t.Fatalf("acceptances = %d, want 1 with a nil admission provider", len(ch))
	}
}

// TestAcceptanceFiresOnce: the observer sends at most once per request,
// however many times the handoff point is crossed.
func TestAcceptanceFiresOnce(t *testing.T) {
	pu := admissionUnit(t)
	obs, ch := wiredObserver(4)
	ctx := acceptCtx(obs)
	handoff(t, pu, ctx, acceptResp)
	handoff(t, pu, ctx, acceptResp)
	if len(ch) != 1 {
		t.Fatalf("acceptances = %d, want exactly 1", len(ch))
	}
}

// TestAcceptanceSilentWhenDenied: none of the three admission denials
// is an acceptance — the customer stack never runs, so the inlet must
// keep its claim and retry.
func TestAcceptanceSilentWhenDenied(t *testing.T) {
	cases := []struct {
		name   string
		insert string
		drain  func(h *admissionProviderHandle)
	}{
		{"suspended",
			`INSERT INTO tenant_runtime_state (tenant_id, suspended, deny_status) VALUES ('tnt_acme', 1, 402)`,
			nil},
		{"rate_limited",
			`INSERT INTO tenant_runtime_state (tenant_id, rate_limit_rps, rate_burst) VALUES ('tnt_acme', 1, 1)`,
			func(h *admissionProviderHandle) { h.p.AllowRate("acme") }},
		{"at_capacity",
			`INSERT INTO tenant_runtime_state (tenant_id, concurrency_limit) VALUES ('tnt_acme', 1)`,
			func(h *admissionProviderHandle) { h.p.AcquireConcurrency("acme", admission.NewLease()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pu := admissionUnit(t)
			h := limitsProvider(t, pu, tc.insert)
			if tc.drain != nil {
				tc.drain(h)
			}
			obs, ch := wiredObserver(1)
			if stop := handoff(t, pu, acceptCtx(obs), acceptResp); !stop {
				t.Fatal("denial must stop the run")
			}
			if len(ch) != 0 {
				t.Fatalf("acceptances = %d, want 0 on %s", len(ch), tc.name)
			}
		})
	}
}

// TestAcceptanceSilentWhenRunStaysInSys: a boot stage that advances
// without re-tenanting never accepts — nothing was routed to a tenant.
func TestAcceptanceSilentWhenRunStaysInSys(t *testing.T) {
	pu := admissionUnit(t)
	obs, ch := wiredObserver(1)
	handoff(t, pu, acceptCtx(obs), `{"_txc":{"goto":"boot/200"}}`)
	if len(ch) != 0 {
		t.Fatalf("acceptances = %d, want 0 for a _sys-only run", len(ch))
	}
}

// TestAcceptanceSilentWhenTenantUnknown: maybeRetenant rejects a slug
// with no tenants row, so the pin never changes and nothing is accepted.
func TestAcceptanceSilentWhenTenantUnknown(t *testing.T) {
	pu := admissionUnit(t)
	obs, ch := wiredObserver(1)
	handoff(t, pu, acceptCtx(obs), `{"_txc":{"tenant":"nobody","stack":"web","goto":"nobody/web/0"}}`)
	if len(ch) != 0 {
		t.Fatalf("acceptances = %d, want 0 for an unknown tenant", len(ch))
	}
}

// TestAcceptanceNilSafe: an inlet that sets no channel, an observer that
// was never attached, and a nil observer are all silent no-ops at the
// handoff — every other inlet keeps working exactly as before.
func TestAcceptanceNilSafe(t *testing.T) {
	var none *TenantObserver
	none.NotifyAccepted(nil)
	none.accepted()
	NewTenantObserver().accepted()

	pu := admissionUnit(t)
	obs := NewTenantObserver() // attached, no channel
	handoff(t, pu, acceptCtx(obs), acceptResp)
	if slug, ok := obs.Tenant(); !ok || slug != "acme" {
		t.Fatalf("tenant = %q/%v, want acme pinned", slug, ok)
	}
	// No observer on ctx at all (tests, CLI).
	handoff(t, pu, WithTenant(context.Background(), tenants.SystemTenantSlug), acceptResp)
}

// TestRunScope: the trusted continuation run id — a resumed run, a
// deferred-join run, or "" on an ordinary request.
func TestRunScope(t *testing.T) {
	if got := RunScope(context.Background()); got != "" {
		t.Fatalf("ordinary run = %q, want empty", got)
	}
	if got := RunScope(withResumeRun(context.Background(), resumeIdent{runID: "run_r", rcid: "rc"})); got != "run_r" {
		t.Fatalf("resumed = %q, want run_r", got)
	}
	if got := RunScope(withDeferredRun(context.Background(), "run_d", "rc")); got != "run_d" {
		t.Fatalf("deferred = %q, want run_d", got)
	}
}
