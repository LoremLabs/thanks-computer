package processor

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// ffCaptureTracer records Steps and TimelineEvents so tests can assert
// which ops actually fired and that the walk emitted stage.fastforward.
type ffCaptureTracer struct {
	mu     sync.Mutex
	steps  []trace.StepInfo
	events []trace.TimelineEvent
}

func (t *ffCaptureTracer) Step(info trace.StepInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, info)
}
func (t *ffCaptureTracer) Event(ev trace.TimelineEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, ev)
}
func (t *ffCaptureTracer) End(string, string, []byte) {}

func (t *ffCaptureTracer) stepKeys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.steps))
	for _, s := range t.steps {
		keys = append(keys, s.Stack+"/"+strconv.Itoa(s.Scope)+" "+s.Name)
	}
	return keys
}

func (t *ffCaptureTracer) fastForwardEvents() []trace.TimelineEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []trace.TimelineEvent
	for _, e := range t.events {
		if e.Event == "stage.fastforward" {
			out = append(out, e)
		}
	}
	return out
}

// seedDeadWalk populates `www` for tenant alpha with n dead scopes
// (WHENs that never match) starting at 1000, stepping 100.
func seedDeadWalk(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	seedIdxTenant(t, db, "t-a", "alpha")
	for i := 0; i < n; i++ {
		scope := 1000 + i*100
		seedIdxOp(t, db, "t-a", "www", scope, fmt.Sprintf("dead%d", i),
			fmt.Sprintf(`WHEN .path == "/never-%d" EXEC "txco://noop"`, i))
	}
}

// runOnce drives a full pu.Run and returns the final emitted payload.
// oracle=true pins the request to the second file handle, which
// disables the ops index AND the fast-forward gate — the byte-exact
// pre-fast-forward frame-per-scope path.
func runOnce(t *testing.T, pu *Unit, sqlHandle *sql.DB, tracer *ffCaptureTracer, raw, stage string, oracle bool) string {
	t.Helper()
	ctx := WithTenant(context.Background(), "alpha")
	if oracle {
		ctx = context.WithValue(ctx, ctxKeyOpstackSnap, sqlHandle)
	}
	if tracer != nil {
		ctx = trace.WithContext(ctx, tracer)
	}
	resCh := make(chan event.Payload, 4)
	if err := pu.Run(ctx, raw, stage, resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		return p.Raw
	default:
		t.Fatal("Run emitted no payload")
		return ""
	}
}

// normalizeSeen sorts the `_txc._seen` array (serialized from a map, so
// its order is nondeterministic) and returns (normalized payload, seen
// set) for byte comparison.
func normalizeSeen(t *testing.T, payload string) (string, []string) {
	t.Helper()
	var seen []string
	for _, v := range gjson.Get(payload, "_txc._seen").Array() {
		seen = append(seen, v.String())
	}
	sort.Strings(seen)
	out, err := sjson.Delete(payload, "_txc._seen")
	if err != nil {
		t.Fatalf("delete _seen: %v", err)
	}
	return out, seen
}

// assertParity runs the same request through the fast-forward path and
// the oracle path and requires identical final payloads (including
// budget fields — the fuel/TTL parity proof).
func assertParity(t *testing.T, pu *Unit, sqlHandle *sql.DB, raw, stage string) (ff, oracle *ffCaptureTracer) {
	t.Helper()
	ff, oracle = &ffCaptureTracer{}, &ffCaptureTracer{}
	got := runOnce(t, pu, sqlHandle, ff, raw, stage, false)
	want := runOnce(t, pu, sqlHandle, oracle, raw, stage, true)

	gotN, gotSeen := normalizeSeen(t, got)
	wantN, wantSeen := normalizeSeen(t, want)
	if gotN != wantN {
		t.Fatalf("payload mismatch:\n ff:     %s\n oracle: %s", gotN, wantN)
	}
	if fmt.Sprint(gotSeen) != fmt.Sprint(wantSeen) {
		t.Fatalf("_seen mismatch:\n ff:     %v\n oracle: %v", gotSeen, wantSeen)
	}
	if fk, ok := fmt.Sprint(ff.stepKeys()), fmt.Sprint(oracle.stepKeys()); fk != ok {
		t.Fatalf("fired-op mismatch:\n ff:     %s\n oracle: %s", fk, ok)
	}
	return ff, oracle
}

func TestFastForward404WalkParity(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedDeadWalk(t, pu.Dbc.Db, 96)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 900000, "catch-all",
		`WHEN .path =~ /.*/  EMIT .status = 404, @halt = true`)

	raw := `{"path":"/miss/xyz"}`
	ff, _ := assertParity(t, pu, sqlHandle, raw, "www/0")

	// Only the catch-all fired.
	if keys := ff.stepKeys(); len(keys) != 1 {
		t.Fatalf("want exactly the catch-all step, got %v", keys)
	}
	// The whole walk collapsed into one stage.fastforward event with the
	// right shape: 96 dead scopes hopped, 97 scope-sets evaluated.
	evs := ff.fastForwardEvents()
	if len(evs) != 1 {
		t.Fatalf("want 1 stage.fastforward event, got %d", len(evs))
	}
	f := evs[0].Fields
	if f["scopes"] != 96 || f["ops_evaluated"] != 97 {
		t.Fatalf("fastforward fields = %v, want scopes=96 ops_evaluated=97", f)
	}
	if f["from"] != "www/0" || f["to"] != "www/900000" {
		t.Fatalf("fastforward from/to = %v/%v", f["from"], f["to"])
	}
}

func TestFastForwardMidStackFiresAndAdvances(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedDeadWalk(t, pu.Dbc.Db, 20)
	// Two live ops mid-walk (neither halts) + a halting catch-all. The
	// oracle comparison proves each fires exactly once and advancement
	// after a fast-forward landing goes strictly FORWARD (a stale
	// entry-computed nextOps would re-enter scope 1000 and loop).
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 2250, "hit-a", `WHEN .path == "/hit" EMIT .a = "1"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 2610, "hit-b", `WHEN .a == "1" EMIT .b = "2"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 900000, "done",
		`WHEN .path =~ /.*/  EMIT .status = 200, @halt = true`)

	ff, _ := assertParity(t, pu, sqlHandle, `{"path":"/hit"}`, "www/0")

	if keys := ff.stepKeys(); len(keys) != 3 {
		t.Fatalf("want hit-a, hit-b, done to fire once each, got %v", keys)
	}
}

func TestFastForwardEndOfStack(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedDeadWalk(t, pu.Dbc.Db, 12)
	// No catch-all: the walk exhausts the ladder and the default branch
	// emits the (unmodified, budget-synced) envelope as the final body.
	ff, _ := assertParity(t, pu, sqlHandle, `{"path":"/nothing"}`, "www/0")
	if keys := ff.stepKeys(); len(keys) != 0 {
		t.Fatalf("no op should fire, got %v", keys)
	}
	if evs := ff.fastForwardEvents(); len(evs) != 1 || evs[0].Fields["scopes"] != 12 {
		t.Fatalf("want one fastforward event over 12 scopes, got %v", evs)
	}
}

func TestFastForwardBreakpointGateKeepsOldPath(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedDeadWalk(t, pu.Dbc.Db, 20)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 900000, "catch-all",
		`WHEN .path =~ /.*/  EMIT .status = 404, @halt = true`)

	// Breakpoint at 1500: the walk must halt at the same pre-threshold
	// scope on both paths (fast-forward is gated OFF by the flag).
	raw := `{"path":"/miss","_txc":{"flag_breakpoint":true,"break":1500}}`
	ff, _ := assertParity(t, pu, sqlHandle, raw, "www/0")

	got := runOnce(t, pu, sqlHandle, nil, raw, "www/0", false)
	if !gjson.Get(got, "_txc.broke_at").Exists() {
		t.Fatalf("breakpoint did not fire: %s", got)
	}
	if evs := ff.fastForwardEvents(); len(evs) != 0 {
		t.Fatalf("fast-forward ran despite breakpoint flag: %v", evs)
	}
}

func TestFastForwardTTLExhaustionParity(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedDeadWalk(t, pu.Dbc.Db, 40)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 900000, "catch-all",
		`WHEN .path =~ /.*/  EMIT .status = 404, @halt = true`)
	pu.Conf.OpScopeTTLMax = 10 // exhausts mid-walk

	ffOut := runOnce(t, pu, sqlHandle, nil, `{"path":"/miss"}`, "www/0", false)
	orOut := runOnce(t, pu, sqlHandle, nil, `{"path":"/miss"}`, "www/0", true)
	if ffOut != orOut {
		t.Fatalf("exhaustion payload mismatch:\n ff:     %s\n oracle: %s", ffOut, orOut)
	}
	if gjson.Get(ffOut, "code").String() != "txcl_scope_ttl_exhausted" {
		t.Fatalf("want TTL exhaustion, got %s", ffOut)
	}
}

func BenchmarkRunWalk(b *testing.B) {
	// "frames" = index-backed lookups but frame-per-scope recursion (the
	// breakpoint flag gates fast-forward off without a break scope set) —
	// i.e. the state after the ops-index fix but before fast-forward.
	for _, mode := range []string{"fastforward", "frames", "oracle"} {
		b.Run(mode, func(b *testing.B) {

			pu, sqlHandle := newFileBackedUnit(b)
			seedIdxTenant(b, pu.Dbc.Db, "t-a", "alpha")
			for i := 0; i < 96; i++ {
				seedIdxOp(b, pu.Dbc.Db, "t-a", "www", 1000+i*100, fmt.Sprintf("dead%d", i),
					fmt.Sprintf(`WHEN .path == "/never-%d" EXEC "txco://noop"`, i))
			}
			seedIdxOp(b, pu.Dbc.Db, "t-a", "www", 900000, "catch-all",
				`WHEN .path =~ /.*/  EMIT .status = 404, @halt = true`)

			ctx := WithTenant(context.Background(), "alpha")
			if mode == "oracle" {
				ctx = context.WithValue(ctx, ctxKeyOpstackSnap, sqlHandle)
			}
			raw := `{"path":"/miss"}`
			if mode == "frames" {
				raw = `{"path":"/miss","_txc":{"flag_breakpoint":true}}`
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resCh := make(chan event.Payload, 4)
				if err := pu.Run(ctx, raw, "www/0", resCh); err != nil {
					b.Fatalf("Run: %v", err)
				}
				<-resCh
			}
		})
	}
}
