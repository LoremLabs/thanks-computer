package processor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kvtools/valkeyrie"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/allowance"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/kv"
	boltdb "github.com/loremlabs/thanks-computer/chassis/kv/boltstore"
)

// allowanceUnit is a test unit with a real allowance store behind a
// txco://allow-enter stub that does what the server's txco://allowance/enter
// does: read the allowance's window, then pin the request to it.
func allowanceUnit(t *testing.T, maxFuel int) (*Unit, *allowance.Store) {
	t.Helper()
	pu := withBudget(t, maxFuel, 0, 0)
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	store := allowance.NewStore(kv.New(s, 0, 0))
	pu.Handle([]byte("txco://allow-enter"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			st, err := store.Status(ctx, "acme", "scout")
			if err != nil {
				return event.Payload{}, err
			}
			if _, err := EnterAllowance(ctx, AllowanceCheck{
				Name: st.Name, Defined: st.Defined, Fuel: st.Fuel, Used: st.Used, ResetsAt: st.ResetsAt,
			}); err != nil {
				return event.Payload{}, err
			}
			return event.Payload{Raw: `{}`, Type: event.JSON}, nil
		}))
	return pu, store
}

func runAllowance(t *testing.T, pu *Unit, in, stage string) event.Payload {
	t.Helper()
	resCh := make(chan event.Payload, 4)
	if err := pu.Run(context.Background(), in, stage, resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		return p
	default:
		t.Fatal("expected a payload on resCh")
		return event.Payload{}
	}
}

// A request that enters a spent allowance is refused at its next scope
// entry: the shared admission marker (429, allowance_exhausted, Retry-After
// = the window's end), the error body, and what the request burned before
// entering — which bills, against the allowance.
func TestAllowanceSpentRefusesAtNextScope(t *testing.T) {
	pu, store := allowanceUnit(t, 0)
	ctx := context.Background()
	if err := store.Set(ctx, "acme", allowance.Def{Name: "scout", Fuel: 500, Per: allowance.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := store.Charge(ctx, "acme", "scout", 500); err != nil {
		t.Fatal(err)
	}
	seedBudgetOp(t, pu, "boot/a", 0, `EXEC "txco://allow-enter"`)
	seedBudgetOp(t, pu, "boot/a", 1, `EMIT .reached = true`)

	p := runAllowance(t, pu, `{"_txc":{"stack":"a"}}`, "boot/a/0")
	if code := gjson.Get(p.Raw, "code").String(); code != "txco_allowance_exhausted" {
		t.Fatalf("code = %q, want txco_allowance_exhausted (payload: %s)", code, p.Raw)
	}
	if gjson.Get(p.Raw, "reached").Exists() {
		t.Error("the scope after the refusal ran")
	}
	status, reason, denied := 0, "", false
	if s, r, ok := admissionMarker(p.Raw); ok {
		status, reason, denied = s, r, ok
	}
	if !denied || status != 429 || reason != AllowanceDenyReason {
		t.Errorf("admission marker = (%v, %d, %q), want denied 429 %s", denied, status, reason, AllowanceDenyReason)
	}
	_, _, end := allowance.Hour.Window(time.Now())
	ra := gjson.Get(p.Raw, "_txc.admission.retry_after").Int()
	if want := int64(time.Until(end).Seconds()); ra < want-5 || ra > want+5 {
		t.Errorf("retry_after = %d, want ~%d (seconds to the window's end)", ra, want)
	}
	if FuelUsedFromEnvelope(p.Raw) <= 0 {
		t.Errorf("refused request bills no fuel: %s", p.Raw)
	}
	if a := AllowanceFromEnvelope(p.Raw); a != "scout" {
		t.Errorf("allowance on the refusal = %q, want scout", a)
	}
}

func admissionMarker(raw string) (int, string, bool) {
	if !gjson.Get(raw, "_txc.admission.denied").Bool() {
		return 0, "", false
	}
	return int(gjson.Get(raw, "_txc.admission.status").Int()), gjson.Get(raw, "_txc.admission.reason").String(), true
}

// Entering an allowance with fuel left lowers the request's ceiling to what
// the window has left, so a runaway request is stopped as the allowance's,
// well before the chassis-wide cap.
func TestAllowanceCeilingStopsRunaway(t *testing.T) {
	pu, store := allowanceUnit(t, 100000)
	ctx := context.Background()
	if err := store.Set(ctx, "acme", allowance.Def{Name: "scout", Fuel: 500, Per: allowance.Day}); err != nil {
		t.Fatal(err)
	}
	if err := store.Charge(ctx, "acme", "scout", 300); err != nil {
		t.Fatal(err)
	}
	seedBudgetOp(t, pu, "boot/c", 0, `EXEC "txco://allow-enter"`)
	seedBudgetOp(t, pu, "boot/c", 1, `EMIT @goto = "boot/c/1"`)

	p := runAllowance(t, pu, `{}`, "boot/c/0")
	if code := gjson.Get(p.Raw, "code").String(); code != "txco_allowance_exhausted" {
		t.Fatalf("code = %q, want txco_allowance_exhausted (payload: %s)", code, p.Raw)
	}
	fuel := FuelUsedFromEnvelope(p.Raw)
	if fuel <= 200 || fuel > 400 {
		t.Errorf("fuel_used = %d, want just over the 200 left in the window (+ entry cost)", fuel)
	}
	if c := gjson.Get(p.Raw, "_txc.allowance.cap").Int(); c <= 200 || c > 300 {
		t.Errorf("_txc.allowance.cap = %d, want fuel-at-entry + 200", c)
	}
}

// An allowance that was never defined is metered, never refused, and the
// pin rides the final payload for the usage line.
func TestAllowanceUndefinedIsMeteredOnly(t *testing.T) {
	pu, _ := allowanceUnit(t, 0)
	seedBudgetOp(t, pu, "boot/u", 0, `EXEC "txco://allow-enter"`)
	seedBudgetOp(t, pu, "boot/u", 1, `EMIT .reached = true`)

	p := runAllowance(t, pu, `{}`, "boot/u/0")
	if !gjson.Get(p.Raw, "reached").Bool() {
		t.Fatalf("undefined allowance stopped the request: %s", p.Raw)
	}
	if a := AllowanceFromEnvelope(p.Raw); a != "scout" {
		t.Errorf("final payload allowance = %q, want scout", a)
	}
	if gjson.Get(p.Raw, "_txc.allowance.cap").Exists() {
		t.Errorf("undefined allowance set a ceiling: %s", p.Raw)
	}
}

// Entering is write-once: another allowance is refused, the same one
// refreshes.
func TestEnterAllowanceWriteOnce(t *testing.T) {
	pu := withBudget(t, 0, 0, 0)
	ctx, _, _ := loadBudget(context.Background(), `{}`, pu.Conf)
	if _, err := EnterAllowance(ctx, AllowanceCheck{Name: "scout"}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnterAllowance(ctx, AllowanceCheck{Name: "bard"}); !errors.Is(err, ErrAllowanceEntered) {
		t.Errorf("second allowance: err = %v, want ErrAllowanceEntered", err)
	}
	if _, err := EnterAllowance(ctx, AllowanceCheck{Name: "scout"}); err != nil {
		t.Errorf("re-entering the same allowance: %v", err)
	}
	if got := AllowanceScope(ctx); got != "scout" {
		t.Errorf("AllowanceScope = %q, want scout", got)
	}
	if _, err := EnterAllowance(context.Background(), AllowanceCheck{Name: "scout"}); err == nil {
		t.Error("entering with no request budget succeeded")
	}
}

// The pin and its ceiling survive the envelope round trip a continuation or
// a detached op takes, and the ceiling only ever lowers the chassis cap.
func TestAllowancePinSurvivesEnvelope(t *testing.T) {
	pu := withBudget(t, 100000, 0, 0)
	ctx, _, _ := loadBudget(context.Background(), `{}`, pu.Conf)
	_ = addFuel(ctx, 40, "x")
	resets := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := EnterAllowance(ctx, AllowanceCheck{Name: "scout", Defined: true, Fuel: 500, Used: 100, ResetsAt: resets}); err != nil {
		t.Fatal(err)
	}
	raw := syncBudgetToEnvelope(ctx, `{}`)

	ctx2, _, _ := loadBudget(context.Background(), raw, pu.Conf)
	if got := AllowanceScope(ctx2); got != "scout" {
		t.Fatalf("restored allowance = %q, want scout", got)
	}
	s := budgetFromCtx(ctx2)
	if m := s.maxFuel.Load(); m != 440 {
		t.Errorf("restored ceiling = %d, want 440 (40 burned + 400 left)", m)
	}
	err := s.overBudget(441, "y")
	var ae *AllowanceExhaustedError
	if !errors.As(err, &ae) || !ae.ResetsAt.Equal(resets) || ae.Fuel != 500 {
		t.Errorf("over the restored ceiling: err = %v, want the allowance's (resets %s)", err, resets)
	}

	// A chassis cap below the allowance's ceiling stays in force.
	pu.Conf.MaxFuelPerRequest = 300
	ctx3, _, _ := loadBudget(context.Background(), raw, pu.Conf)
	if m := budgetFromCtx(ctx3).maxFuel.Load(); m != 300 {
		t.Errorf("ceiling with a lower chassis cap = %d, want 300", m)
	}
}

// `_txc.allowance` is chassis-owned: a rule cannot write it, so it cannot
// put a request in an allowance (or out of one) by EMIT.
func TestRuleCannotWriteAllowance(t *testing.T) {
	pu := withBudget(t, 0, 0, 0)
	seedBudgetOp(t, pu, "boot/w", 0, `EMIT @allowance = "scout"`)
	p := runAllowance(t, pu, `{}`, "boot/w/0")
	if gjson.Get(p.Raw, "_txc.allowance").Exists() {
		t.Errorf("a rule wrote _txc.allowance: %s", p.Raw)
	}
}
