package processor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// withBudget wraps newTestUnit and stamps the three budget knobs onto the
// returned Unit's config. Zero values disable enforcement (and rule out
// any cross-test interference from the chassis defaults that aren't yet
// set in the test helper).
func withBudget(t *testing.T, maxFuel, maxTTL, penaltyMs int) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	pu.Conf.MaxFuelPerRequest = maxFuel
	pu.Conf.OpScopeTTLMax = maxTTL
	pu.Conf.OpRepeatPenaltyMs = penaltyMs
	return pu
}

// seedBudgetOp is a convenience helper around the INSERT used by the other
// processor tests. Keeps the budget-test bodies focused on assertions.
func seedBudgetOp(t *testing.T, pu *Unit, stack string, scope int, txcl string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		stack, scope, txcl,
	); err != nil {
		t.Fatalf("seed %s/%d: %v", stack, scope, err)
	}
}

// TestRunFuelAccumulatesAcrossScopes drives Run through a five-scope
// pipeline whose rules do nothing but advance, confirming that fuel
// accrues at the scope-enter floor of 10/hop. The chassis-emitted final
// response carries `_txc.fuel_used` (server.go owns the user-facing
// strip), so we read it directly off the payload.
func TestRunFuelAccumulatesAcrossScopes(t *testing.T) {
	pu := withBudget(t, 0, 0, 0) // metering only — no enforcement, no penalty
	for _, scope := range []int{0, 1, 2, 3, 4} {
		seedBudgetOp(t, pu, "boot/flat", scope, `EMIT .marker = "ok"`)
	}

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/flat/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case p := <-resCh:
		// Five Run entries × 10 = 50 (no EXEC clauses, no secrets, no
		// repeats). Bands allow small over-counts from any internal
		// natural-advancement walks.
		fuel := gjson.Get(p.Raw, "_txc.fuel_used").Int()
		if fuel < 40 || fuel > 100 {
			t.Errorf("fuel after 5-scope flat run = %d, want ~50 (5 × scope-enter)", fuel)
		}
	default:
		t.Fatal("expected a payload on resCh")
	}
}

// TestRunTTLExhaustionTightLoop tightens OpScopeTTLMax to 5, fires a
// tight `boot/0 → boot/0` self-loop, and asserts the request terminates
// with a structured txcl_scope_ttl_exhausted payload. Fuel enforcement is
// disabled so we know TTL caught it (TTL fires first at hop 5; fuel at the
// same hops would only have accrued ~10+50+50+50+50 = 210 ≪ 100k default).
func TestRunTTLExhaustionTightLoop(t *testing.T) {
	pu := withBudget(t, 0, 5, 0) // TTL=5, no fuel cap, no penalty sleep
	seedBudgetOp(t, pu, "boot/loop", 0, `EMIT @goto = "boot/loop/0"`)

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/loop/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case p := <-resCh:
		code := gjson.Get(p.Raw, "code").String()
		if code != "txcl_scope_ttl_exhausted" {
			t.Errorf("expected code=txcl_scope_ttl_exhausted, got %q (payload: %s)", code, p.Raw)
		}
		if v := gjson.Get(p.Raw, "max_ttl").Int(); v != 5 {
			t.Errorf("max_ttl = %d, want 5", v)
		}
		// last_transitions should mention the loop endpoint.
		lt := gjson.Get(p.Raw, "last_transitions").Array()
		if len(lt) == 0 {
			t.Errorf("expected non-empty last_transitions, got: %s", p.Raw)
		}
	default:
		t.Fatal("expected an exhaustion payload on resCh")
	}
}

// TestRunFuelExhaustionTightLoop disables TTL and tightens fuel so that
// the same tight self-loop terminates with a structured
// txco_fuel_exhausted payload. Confirms the fuel path is wired and that
// the two error codes are distinct.
func TestRunFuelExhaustionTightLoop(t *testing.T) {
	// Fuel = 200; tight loop costs 10 + 50 per hop after first ≈ 60/hop.
	// Exhausts within ~5 hops, well below any natural Run depth limit.
	pu := withBudget(t, 200, 0, 0) // fuel=200, TTL disabled, no sleep
	seedBudgetOp(t, pu, "boot/loop", 0, `EMIT @goto = "boot/loop/0"`)

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/loop/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case p := <-resCh:
		code := gjson.Get(p.Raw, "code").String()
		if code != "txco_fuel_exhausted" {
			t.Errorf("expected code=txco_fuel_exhausted, got %q (payload: %s)", code, p.Raw)
		}
		if v := gjson.Get(p.Raw, "max_fuel").Int(); v != 200 {
			t.Errorf("max_fuel = %d, want 200", v)
		}
		if v := gjson.Get(p.Raw, "fuel_used").Int(); v <= 200 {
			t.Errorf("fuel_used = %d, want > 200", v)
		}
	default:
		t.Fatal("expected an exhaustion payload on resCh")
	}
}

// TestRunRepeatPenaltySleeps sets a 30ms penalty per repeated transition
// and asserts the request takes at least that long when the loop iterates.
// Wired-vs-disabled check (compared with the previous TTL test, where
// penalty=0).
func TestRunRepeatPenaltySleeps(t *testing.T) {
	// TTL=6: hop1 has no parent (no transition charge), hop2 is the
	// first time we see boot/0→boot/0 (no repeat), hops 3-5 are repeats
	// and each pays a 30ms penalty (≈90ms total), hop 6 exhausts TTL
	// before paying its sleep. Floor at 60ms is conservative.
	pu := withBudget(t, 0, 6, 30)
	seedBudgetOp(t, pu, "boot/loop", 0, `EMIT @goto = "boot/loop/0"`)

	start := time.Now()
	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/loop/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 60*time.Millisecond {
		t.Errorf("elapsed = %v; expected ≥ 60ms from repeat-transition penalty sleeps", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v; expected ≤ 500ms (no per-iteration cost blowup)", elapsed)
	}

	// Drain.
	select {
	case <-resCh:
	default:
	}
}

// TestRunRepeatPenaltyDisabledWhenZero is the negative control for the
// above: setting OpRepeatPenaltyMs=0 should fully disable the sleep, so
// the same loop completes promptly.
func TestRunRepeatPenaltyDisabledWhenZero(t *testing.T) {
	pu := withBudget(t, 0, 4, 0)
	seedBudgetOp(t, pu, "boot/loop", 0, `EMIT @goto = "boot/loop/0"`)

	start := time.Now()
	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/loop/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Millisecond {
		t.Errorf("elapsed = %v; expected ≤ 30ms (penalty=0 disables sleep)", elapsed)
	}
	select {
	case <-resCh:
	default:
	}
}

// TestRunBudgetStrippedFromOutbound asserts the internal accounting fields
// (_txc.fuel_used, _txc.ttl, _txc._seen) never leak into the response
// delivered through resCh. This guards the strip path; without it,
// clients would see chassis-internal state.
//
// Note: the chassis emits with the fields PRESENT; the strip happens at
// the server convergence point (server.go's runPipeline tee). For this
// test we run the processor directly, so we observe the pre-strip
// payload. We still verify the strip helper works on the bytes the test
// captures, modeling the end-to-end behavior.
func TestRunBudgetStrippedFromOutbound(t *testing.T) {
	pu := withBudget(t, 0, 0, 0)
	seedBudgetOp(t, pu, "boot/flat", 0, `EMIT .marker = "ok"`)

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/flat/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		// Pre-strip (chassis-emitted) should still carry fuel_used so the
		// downstream usage event can read it; the user-facing strip
		// happens further out, in server.go.
		if v := gjson.Get(p.Raw, "_txc.fuel_used"); !v.Exists() {
			t.Errorf("chassis-emitted payload missing _txc.fuel_used (needed for UsageEvent.Fuel): %s", p.Raw)
		}
		// And StripBudgetFromOutbound should remove it.
		stripped := StripBudgetFromOutbound(p.Raw)
		if v := gjson.Get(stripped, "_txc.fuel_used"); v.Exists() {
			t.Errorf("StripBudgetFromOutbound left _txc.fuel_used in: %s", stripped)
		}
		if v := gjson.Get(stripped, "_txc.ttl"); v.Exists() {
			t.Errorf("StripBudgetFromOutbound left _txc.ttl in: %s", stripped)
		}
		if v := gjson.Get(stripped, "_txc._seen"); v.Exists() {
			t.Errorf("StripBudgetFromOutbound left _txc._seen in: %s", stripped)
		}
	default:
		t.Fatal("expected a payload on resCh")
	}
}

// TestRunRuleWriteToFuelUsedSilentlyDropped fires a rule that emits
// `@fuel_used = 0`. The OverlayResponse guard should silently ignore
// the write — fuel only goes up; rules can't decrement or reset it.
func TestRunRuleWriteToFuelUsedSilentlyDropped(t *testing.T) {
	pu := withBudget(t, 0, 0, 0)
	// EMIT @fuel_used = 0 — should be dropped at OverlayResponse.
	seedBudgetOp(t, pu, "boot/flat", 0, `EMIT @fuel_used = 0`)

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/flat/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		// Real fuel accrued from scope-enter should still be >= 10.
		v := gjson.Get(p.Raw, "_txc.fuel_used").Int()
		if v < 10 {
			t.Errorf("_txc.fuel_used = %d, want ≥ 10 (rule write should be dropped)", v)
		}
	default:
		t.Fatal("expected a payload on resCh")
	}
}

// TestRunRuleWriteToTTLClamped fires a rule that tries to raise @ttl.
// Per the IP-TTL idiom, rules can lower their sub-budget but never raise
// it. The override should clamp to the current envelope value.
func TestRunRuleWriteToTTLClamped(t *testing.T) {
	pu := withBudget(t, 0, 100, 0)
	seedBudgetOp(t, pu, "boot/flat", 0, `EMIT @ttl = 999999`)

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/flat/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		v := gjson.Get(p.Raw, "_txc.ttl").Int()
		// TTL started at 100, decremented to 99 on scope-enter; rule
		// tried to raise to 999999 but was clamped to <= 99.
		if v > 99 {
			t.Errorf("_txc.ttl = %d, want ≤ 99 (rule attempt to raise should clamp)", v)
		}
	default:
		t.Fatal("expected a payload on resCh")
	}
}

// TestStripBudgetFromOutboundIdempotent — invoked on a payload that
// doesn't contain the budget fields should produce the same string back.
// Just a small unit test on the helper to lock its semantics.
func TestStripBudgetFromOutboundIdempotent(t *testing.T) {
	in := `{"ok":true,"_txc":{"rid":"abc"}}`
	out := StripBudgetFromOutbound(in)
	if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"rid":"abc"`) {
		t.Errorf("strip mangled untouched fields: %s", out)
	}
	if strings.Contains(out, "fuel_used") || strings.Contains(out, `"ttl"`) || strings.Contains(out, `"_seen"`) {
		t.Errorf("strip introduced budget fields where none existed: %s", out)
	}
}

// TestFuelUsedFromEnvelopeMissing — returns 0 when the field is absent.
func TestFuelUsedFromEnvelopeMissing(t *testing.T) {
	if v := FuelUsedFromEnvelope(`{"x":1}`); v != 0 {
		t.Errorf("FuelUsedFromEnvelope on payload without field = %d, want 0", v)
	}
	if v := FuelUsedFromEnvelope(`{"_txc":{"fuel_used":12345}}`); v != 12345 {
		t.Errorf("FuelUsedFromEnvelope = %d, want 12345", v)
	}
}

