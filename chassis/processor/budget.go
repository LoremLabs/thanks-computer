// Per-request loop / cost guards, working as a pair.
//
//   TTL (`_txc.ttl`)        — cheap, loop-shape-specific hop counter. Decrements
//                             once per `Run` entry. Exhaustion = "this is a loop".
//   Fuel (`_txc.fuel_used`) — weighted per-action work meter. Charges differently
//                             for scope-enter, repeat-transition, EXEC, and secret
//                             materialization. Exhaustion = "this is expensive work".
//
// Both ride the envelope and propagate through goto, EXEC, continuations, and
// deferred-join fan-out the same way `_txc.tenant` does. State lives in the
// envelope (`_txc.fuel_used`, `_txc.ttl`, `_txc._seen`) and is mirrored in a
// per-request context value for atomic mid-Run accounting. The repeat-transition
// seen-set is shared by fuel's +50 charge and TTL's penalty sleep — same map,
// no duplication.
//
// The chassis defaults are loose; deployments that want per-tenant ceilings can
// tighten via config. The conditional emit on the `usage` log line is universal
// (single-tenant deployments see a `fuel=N` field they can ignore; tenant-aware
// deployments aggregate it for billing or quota).
//
// See thanks-computer-service/docs/todo-txcl-fuel-metering.md (fuel model) and
// todo-txcl-loops-and-cycles.md (loop analysis + TTL/penalty rationale).

package processor

import (
	"github.com/loremlabs/thanks-computer/chassis/jsonx"

	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// Cost table — v1 baked-in constants. v2 may promote to config keys if real
// deployments need per-tenant ratios. The numbers anchor the §3 worked
// examples in the fuel doc; final defaults need traffic measurement.
const (
	fuelCostScopeEnter        int64 = 10
	fuelCostRepeatTransition  int64 = 50
	fuelCostExec              int64 = 25
	fuelCostSecretMaterialize int64 = 100
	// fuelCostComputePerMs charges nano-op wall-clock on top of the flat
	// 25-fuel dispatch in Exec. A 1 ms classifier pays 35 total; a 1 s
	// LLM-wrapping op pays ~10K. Anchors to the §2 calibration in the
	// fuel doc (1 unit ≈ 100 µs of typical chassis work).
	fuelCostComputePerMs int64 = 10
)

// ctxKeyBudget carries the per-request budget state. Idempotent across recursive
// Run calls within the same request — loadBudget returns the existing pointer.
var ctxKeyBudget = ctxKeyType{name: "budget-state"}

// ctxKeyParentStage carries the stage that the current Run was advanced from,
// so the repeat-transition check (and its fuel + penalty) has both endpoints.
// Set by advanceAfterScope before each recursive Run.
var ctxKeyParentStage = ctxKeyType{name: "parent-stage"}

// budgetState is the live per-request accounting record. The fuel and TTL
// counters are atomic so parallel ops within a scope can charge concurrently
// without a lock. The seen-set + recent-ring use a mutex (rare write, never
// hot enough to matter).
type budgetState struct {
	fuel      atomic.Int64
	ttl       atomic.Int64
	maxFuel   int64
	maxTTL    int64
	penaltyMs int64

	mu     sync.Mutex
	seen   map[string]bool
	recent []string // ring of last 3 transitions, for the exhaustion error payload
}

// FuelExhaustedError is the structured terminal error for fuel exhaustion.
// Serialized through the same final-response path as a wall-clock cancel.
type FuelExhaustedError struct {
	MaxFuel         int64    `json:"max_fuel"`
	FuelUsed        int64    `json:"fuel_used"`
	LastStage       string   `json:"last_stage"`
	LastTransitions []string `json:"last_transitions"`
}

func (e *FuelExhaustedError) Error() string {
	return fmt.Sprintf("txco_fuel_exhausted: used %d of %d at %s", e.FuelUsed, e.MaxFuel, e.LastStage)
}

func (e *FuelExhaustedError) AsJSON() string {
	b, _ := json.Marshal(struct {
		Code            string   `json:"code"`
		MaxFuel         int64    `json:"max_fuel"`
		FuelUsed        int64    `json:"fuel_used"`
		LastStage       string   `json:"last_stage"`
		LastTransitions []string `json:"last_transitions"`
	}{"txco_fuel_exhausted", e.MaxFuel, e.FuelUsed, e.LastStage, e.LastTransitions})
	return string(b)
}

// TTLExhaustedError is the structured terminal error for hop-budget exhaustion.
// Distinct code from fuel exhaustion so operators can see "this is a loop" vs
// "this is expensive work" at a glance.
type TTLExhaustedError struct {
	MaxTTL          int64    `json:"max_ttl"`
	Consumed        int64    `json:"consumed"`
	LastStage       string   `json:"last_stage"`
	LastTransitions []string `json:"last_transitions"`
}

func (e *TTLExhaustedError) Error() string {
	return fmt.Sprintf("txcl_scope_ttl_exhausted: consumed %d of %d at %s", e.Consumed, e.MaxTTL, e.LastStage)
}

func (e *TTLExhaustedError) AsJSON() string {
	b, _ := json.Marshal(struct {
		Code            string   `json:"code"`
		MaxTTL          int64    `json:"max_ttl"`
		Consumed        int64    `json:"consumed"`
		LastStage       string   `json:"last_stage"`
		LastTransitions []string `json:"last_transitions"`
	}{"txcl_scope_ttl_exhausted", e.MaxTTL, e.Consumed, e.LastStage, e.LastTransitions})
	return string(b)
}

// loadBudget hydrates the per-request budget state from the envelope, attaches
// it to ctx under ctxKeyBudget, and returns the current fuel + TTL values for
// trace stamping. Idempotent: if ctx already carries a state, returns it
// unchanged.
//
// The envelope may carry `_txc.fuel_used` (counter, default 0), `_txc.ttl`
// (countdown, default OpScopeTTLMax), and `_txc._seen` (array of "from->to"
// strings, default empty). All three propagate through goto / EXEC / fan-out
// because they live on the envelope.
func loadBudget(ctx context.Context, raw string, conf config.Config) (context.Context, int64, int64) {
	if existing, ok := ctx.Value(ctxKeyBudget).(*budgetState); ok && existing != nil {
		return ctx, existing.fuel.Load(), existing.ttl.Load()
	}
	s := &budgetState{
		maxFuel:   int64(conf.MaxFuelPerRequest),
		maxTTL:    int64(conf.OpScopeTTLMax),
		penaltyMs: int64(conf.OpRepeatPenaltyMs),
		seen:      map[string]bool{},
	}
	if v := gjson.Get(raw, "_txc.fuel_used"); v.Exists() {
		s.fuel.Store(v.Int())
	}
	if v := gjson.Get(raw, "_txc.ttl"); v.Exists() {
		s.ttl.Store(v.Int())
	} else {
		// First Run for this request — seed TTL from config. 0 means
		// disabled, so we still store 0 and decrementTTL becomes a no-op.
		s.ttl.Store(int64(conf.OpScopeTTLMax))
	}
	if arr := gjson.Get(raw, "_txc._seen"); arr.IsArray() {
		for _, e := range arr.Array() {
			s.seen[e.String()] = true
		}
	}
	ctx = context.WithValue(ctx, ctxKeyBudget, s)
	return ctx, s.fuel.Load(), s.ttl.Load()
}

// budgetFromCtx is an internal accessor used by the charging helpers.
func budgetFromCtx(ctx context.Context) *budgetState {
	if s, ok := ctx.Value(ctxKeyBudget).(*budgetState); ok {
		return s
	}
	return nil
}

// addFuel bumps the request's fuel counter by `cost`. Returns a
// *FuelExhaustedError if the new total exceeds MaxFuelPerRequest > 0.
// `stage` is the current stage, threaded onto the error so operators see
// where the request gave up.
func addFuel(ctx context.Context, cost int64, stage string) error {
	s := budgetFromCtx(ctx)
	if s == nil {
		return nil // accounting not initialized — pre-Run callers (rare); silently allow
	}
	used := s.fuel.Add(cost)
	if s.maxFuel > 0 && used > s.maxFuel {
		s.mu.Lock()
		recent := append([]string(nil), s.recent...)
		s.mu.Unlock()
		return &FuelExhaustedError{
			MaxFuel:         s.maxFuel,
			FuelUsed:        used,
			LastStage:       stage,
			LastTransitions: recent,
		}
	}
	return nil
}

// decrementTTL decrements the request's hop counter by 1. Returns a
// *TTLExhaustedError if the result <= 0 and the cap is enabled (> 0).
// A cap of 0 means "disabled"; loadBudget stored 0 in that case, so this
// would fire immediately — we explicitly skip the check.
func decrementTTL(ctx context.Context, stage string) error {
	s := budgetFromCtx(ctx)
	if s == nil {
		return nil
	}
	// 0 from config = disabled. The store at load time also wrote 0; we
	// keep counting down (informational) but never raise.
	current := s.ttl.Add(-1)
	original := current + 1
	// If the original (pre-decrement) was 0 OR negative, the cap is disabled
	// (or this request was seeded that way from a chained system).
	if original <= 0 {
		return nil
	}
	if current <= 0 {
		s.mu.Lock()
		recent := append([]string(nil), s.recent...)
		s.mu.Unlock()
		return &TTLExhaustedError{
			MaxTTL:          s.maxTTL,
			Consumed:        s.maxTTL,
			LastStage:       stage,
			LastTransitions: recent,
		}
	}
	return nil
}

// chargeTransition records that the request went from `from` to `to`. If
// this transition has been seen before in this request, charges
// fuelCostRepeatTransition and returns the configured repeat penalty for
// the caller to sleep on (context-cancelable). Either way, appends to the
// recent ring and adds to the seen-set.
func chargeTransition(ctx context.Context, from, to string) (time.Duration, error) {
	s := budgetFromCtx(ctx)
	if s == nil {
		return 0, nil
	}
	key := from + "->" + to
	s.mu.Lock()
	repeat := s.seen[key]
	s.seen[key] = true
	s.recent = append(s.recent, key)
	if len(s.recent) > 3 {
		s.recent = s.recent[len(s.recent)-3:]
	}
	penalty := time.Duration(0)
	if repeat && s.penaltyMs > 0 {
		penalty = time.Duration(s.penaltyMs) * time.Millisecond
	}
	s.mu.Unlock()
	if repeat {
		if err := addFuel(ctx, fuelCostRepeatTransition, to); err != nil {
			return 0, err
		}
	}
	return penalty, nil
}

// syncBudgetToEnvelope writes the live fuel + TTL + seen-set back into the
// envelope JSON so the successor `Run` (after goto / EXEC / fan-out) sees
// the updated values. Called just before each recursive Run propagation.
func syncBudgetToEnvelope(ctx context.Context, raw string) string {
	s := budgetFromCtx(ctx)
	if s == nil {
		return raw
	}
	s.mu.Lock()
	seen := make([]string, 0, len(s.seen))
	for k := range s.seen {
		seen = append(seen, k)
	}
	s.mu.Unlock()
	// After the first sync all three paths exist, so this is one
	// splice pass instead of three full-envelope copies per scope hop.
	return jsonx.SetMany(raw, []jsonx.PathVal{
		{Path: "_txc.fuel_used", Val: s.fuel.Load()},
		{Path: "_txc.ttl", Val: s.ttl.Load()},
		{Path: "_txc._seen", Val: seen},
	})
}

// StripBudgetFromOutbound deletes the chassis-internal budget fields from
// the response delivered to the inlet. Called by server.go just before
// forwarding the payload to the inlet client. Mirrors the existing
// `_txc.halt` / `_txc.goto` strip; exported so the server package (which
// owns the convergence between chassis emit and inlet write) can apply it
// after capturing the fuel value for UsageEvent.
func StripBudgetFromOutbound(raw string) string {
	out := raw
	out, _ = sjson.Delete(out, "_txc.fuel_used")
	out, _ = sjson.Delete(out, "_txc.ttl")
	out, _ = sjson.Delete(out, "_txc._seen")
	return out
}

// FuelUsedFromEnvelope reads the live fuel value from an envelope. Used at
// the UsageEvent construction site (server.go:1010) where the merged
// envelope is available but the per-request context is not.
func FuelUsedFromEnvelope(raw string) int64 {
	return gjson.Get(raw, "_txc.fuel_used").Int()
}

// TenantFromEnvelope reads the resolved tenant slug from an envelope's
// `_txc.tenant`. Mirrors FuelUsedFromEnvelope; used at the trace-usage emit
// sites (main + resume paths) to attribute a trace to its tenant — the slug
// admin tenant-scoping filters on.
func TenantFromEnvelope(raw string) string {
	return gjson.Get(raw, "_txc.tenant").String()
}

// clampTTL implements the IP-TTL idiom for rule writes: a rule may
// voluntarily lower its sub-budget (`EMIT @ttl = N`) but may not raise it.
// Called from OverlayResponse when an override targets `_txc.ttl`.
func clampTTL(envelope string, requested int64) int64 {
	current := gjson.Get(envelope, "_txc.ttl").Int()
	if requested < current {
		return requested
	}
	return current
}

// emitBudgetExhausted serializes an exhaustion error into the final response
// payload and pushes it through resCh. Returns true if the error was a
// budget exhaustion (caller swallows the error in that case), false
// otherwise (caller bubbles up).
func emitBudgetExhausted(err error, resCh chan event.Payload) bool {
	var raw string
	switch e := err.(type) {
	case *FuelExhaustedError:
		raw = e.AsJSON()
	case *TTLExhaustedError:
		raw = e.AsJSON()
	default:
		return false
	}
	resCh <- event.Payload{Raw: raw, Type: event.JSON}
	return true
}
