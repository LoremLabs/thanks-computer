package processor

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// The contract of `EXEC "call://…"` (call.go, frame.go): docs/todo-stack-call.md
// §6 in the service repo, one test per line of it.

// callRun runs a request from stage and returns every payload it emitted.
func callRun(t *testing.T, pu *Unit, ctx context.Context, in, stage string) ([]event.Payload, error) {
	t.Helper()
	resCh := make(chan event.Payload, 8)
	err := pu.Run(ctx, in, stage, resCh)
	var out []event.Payload
	for {
		select {
		case p := <-resCh:
			out = append(out, p)
		default:
			return out, err
		}
	}
}

// callFinal runs a request and returns its one final envelope.
func callFinal(t *testing.T, pu *Unit, in, stage string) string {
	t.Helper()
	got, err := callRun(t, pu, context.Background(), in, stage)
	if err != nil {
		t.Fatalf("Run(%s): %v", stage, err)
	}
	if len(got) != 1 {
		t.Fatalf("Run(%s): %d payloads, want one: %+v", stage, len(got), got)
	}
	return got[0].Raw
}

const callerIn = `{"_txc":{"stack":"c","src":"http"},"secret":"s3"}`

// --- the boundary (§2.2) --------------------------------------------------

// A call runs the named stack and only it. app = {100, 200} and app/onboard =
// {100, 150}: a call of app/onboard runs 100 and 150 and comes back; app's
// 200 never runs. A goto into the same stack still inherits 200 (the overlay,
// unchanged). The `_`-named child, which never peeled, behaves the same.
func TestCallRunsOnlyTheNamedStack(t *testing.T) {
	for _, child := range []string{"app/onboard", "app/_onboard"} {
		t.Run(child, func(t *testing.T) {
			pu, _ := newTestUnit(t)
			seedOp(t, pu, "app", 100, "a100", `EMIT .ran.app100 = true`)
			seedOp(t, pu, "app", 200, "a200", `EMIT .ran.app200 = true, ._call.output.leaked = true`)
			seedOp(t, pu, child, 100, "c100", `EMIT .ran.child100 = true`)
			seedOp(t, pu, child, 150, "c150", `EMIT .ran.child150 = true, ._call.output.done = true`)
			seedOp(t, pu, "c", 100, "call", `EXEC "call://`+child+`"`)
			seedOp(t, pu, "c", 200, "after", `EMIT .after = true`)
			seedOp(t, pu, "j", 100, "jump", `EXEC "goto://`+child+`/0"`)

			final := callFinal(t, pu, callerIn, "c/0")
			res := gjson.Get(final, "_call")
			if !res.Get("ok").Bool() {
				t.Fatalf("call not ok: %s", final)
			}
			if !res.Get("output.done").Bool() || res.Get("output.leaked").Exists() {
				t.Errorf("call://%s ran the parent's 200: %s", child, final)
			}
			if !gjson.Get(final, "after").Bool() {
				t.Errorf("the caller's next scope did not run: %s", final)
			}
			if gjson.Get(final, "ran").Exists() {
				t.Errorf("the callee's working fields reached the caller: %s", final)
			}

			if child == "app/onboard" {
				jumped := callFinal(t, pu, `{"_txc":{"stack":"j","src":"http"}}`, "j/0")
				if !gjson.Get(jumped, "ran.app200").Bool() || !gjson.Get(jumped, "ran.child150").Bool() {
					t.Errorf("goto://%s/0 should still inherit app/200 (the overlay): %s", child, jumped)
				}
			}
		})
	}
}

// A bare-scope jump inside the callee stays in it. A jump to another stack
// — a cross-stack @goto, an unschemed EXEC, a trusted op that re-pins the
// stack the way txco://route does — ends the call with txco_call_boundary,
// and the other stack never runs.
func TestCallJumpsStayInside(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Handle([]byte("txco://reroute"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in []byte, out []byte) (event.Payload, error) {
			return event.Payload{Raw: `{"_txc":{"goto":"other/100","stack":"other"}}`, Type: event.JSON}, nil
		}))
	seedOp(t, pu, "other", 100, "o", `EMIT .other = true`)
	seedOp(t, pu, "in", 100, "jump", `EMIT @goto = "300"`)
	seedOp(t, pu, "in", 200, "skipped", `EMIT ._call.output.skipped = true`)
	seedOp(t, pu, "in", 300, "landed", `EMIT ._call.output.landed = true`)
	seedOp(t, pu, "out-goto", 100, "jump", `EMIT @goto = "other/100"`)
	seedOp(t, pu, "out-exec", 100, "jump", `EXEC "other/100"`)
	seedOp(t, pu, "out-route", 100, "jump", `EXEC "txco://reroute"`)
	for _, callee := range []string{"in", "out-goto", "out-exec", "out-route"} {
		seedOp(t, pu, "c-"+callee, 100, "call", `EXEC "call://`+callee+`"`)
		seedOp(t, pu, "c-"+callee, 200, "after", `EMIT .after = true`)
	}

	final := callFinal(t, pu, `{"_txc":{"stack":"c-in","src":"http"}}`, "c-in/0")
	if !gjson.Get(final, "_call.ok").Bool() || !gjson.Get(final, "_call.output.landed").Bool() ||
		gjson.Get(final, "_call.output.skipped").Exists() {
		t.Errorf("bare-scope jump inside the callee: %s", final)
	}
	for _, callee := range []string{"out-goto", "out-exec", "out-route"} {
		final := callFinal(t, pu, `{"_txc":{"stack":"c-`+callee+`","src":"http"}}`, "c-"+callee+"/0")
		if gjson.Get(final, "_call.ok").Bool() {
			t.Errorf("%s: a jump out of the callee should end the call: %s", callee, final)
		}
		if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_boundary" {
			t.Errorf("%s: code = %q, want txco_call_boundary: %s", callee, got, final)
		}
		if gjson.Get(final, "other").Exists() {
			t.Errorf("%s: the other stack ran: %s", callee, final)
		}
		if !gjson.Get(final, "after").Bool() {
			t.Errorf("%s: the caller did not continue: %s", callee, final)
		}
	}
}

// --- the result (§2.4) ----------------------------------------------------

func topKeys(doc string) []string {
	var keys []string
	gjson.Parse(doc).ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	sort.Strings(keys)
	return keys
}

// A call writes exactly one object under `into` and nothing else: the
// caller's envelope gains that key and no other, and `_txc` gains only what
// the budget sync writes on every request.
func TestCallWritesOneObjectUnderInto(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "sub", 100, "s", `EMIT ._call.output.x = 1, .work = "mine", @web.res.status = 418`)
	seedOp(t, pu, "c", 100, "call", `WITH input = &object("a", 1), into = "_orient" EXEC "call://sub"`)
	seedOp(t, pu, "plain", 100, "p", `EMIT .nothing = true`)

	final := callFinal(t, pu, callerIn, "c/0")
	if got, want := topKeys(final), []string{"_orient", "_txc", "secret"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("top-level keys = %v, want %v: %s", got, want, final)
	}
	res := gjson.Get(final, "_orient")
	for _, k := range []string{"ok", "output", "error", "stack", "version", "ms"} {
		if !res.Get(k).Exists() {
			t.Errorf("result lacks %q: %s", k, res.Raw)
		}
	}
	if !res.Get("ok").Bool() || res.Get("output.x").Int() != 1 || res.Get("error").Type != gjson.Null ||
		res.Get("stack").String() != "sub" {
		t.Errorf("result = %s", res.Raw)
	}
	if gjson.Get(final, "_txc.web.res").Exists() {
		t.Errorf("the callee's @web.res reached the caller: %s", final)
	}
	// The caller's _txc has what a request without a call has: compare
	// against a plain request's keys.
	plain := callFinal(t, pu, `{"_txc":{"stack":"plain","src":"http"}}`, "plain/0")
	if got, want := topKeys(gjson.Get(final, "_txc").Raw), topKeys(gjson.Get(plain, "_txc").Raw); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("_txc keys = %v, want a plain request's %v", got, want)
	}
}

func TestCallTwoCallsInOneScopeDoNotCollide(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Dbc.Db.SetMaxOpenConns(1) // concurrent lookups on a :memory: handle
	seedOp(t, pu, "one", 100, "s", `EMIT ._call.output.who = "one"`)
	seedOp(t, pu, "two", 100, "s", `EMIT ._call.error = &object("code", "two_failed", "message", "no")`)
	seedOp(t, pu, "c", 100, "call-one", `WITH into = "_one" EXEC "call://one"`)
	seedOp(t, pu, "c", 100, "call-two", `WITH into = "_two" EXEC "call://two"`)

	final := callFinal(t, pu, callerIn, "c/0")
	if !gjson.Get(final, "_one.ok").Bool() || gjson.Get(final, "_one.output.who").String() != "one" {
		t.Errorf("_one = %s", gjson.Get(final, "_one").Raw)
	}
	if gjson.Get(final, "_two.ok").Bool() || gjson.Get(final, "_two.error.code").String() != "two_failed" ||
		gjson.Get(final, "_two.error.message").String() != "no" {
		t.Errorf("_two = %s", gjson.Get(final, "_two").Raw)
	}
	if gjson.Get(final, "_call").Exists() {
		t.Errorf("a call with WITH into wrote the default key too: %s", final)
	}
}

// ok is the chassis's verdict: false for a `_call.error`, true for a callee
// that wrote neither (a call made for its effects), with output {}.
func TestCallVerdict(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "silent", 100, "s", `EMIT .did = "something"`)
	seedOp(t, pu, "both", 100, "s", `EMIT ._call.output.partial = true, ._call.error.code = "half"`)
	seedOp(t, pu, "text", 100, "s", `EMIT ._call.error = "just a message"`)
	for callee, want := range map[string]string{
		"silent": `ok=true output={} code=`,
		"both":   `ok=false output={"partial":true} code=half`,
		"text":   `ok=false output={} code=txco_call_error`,
	} {
		seedOp(t, pu, "c-"+callee, 100, "call", `EXEC "call://`+callee+`"`)
		final := callFinal(t, pu, `{"_txc":{"stack":"c-`+callee+`","src":"http"}}`, "c-"+callee+"/0")
		res := gjson.Get(final, "_call")
		got := "ok=" + res.Get("ok").Raw + " output=" + res.Get("output").Raw + " code=" + res.Get("error.code").String()
		if got != want {
			t.Errorf("%s: %s, want %s: %s", callee, got, want, res.Raw)
		}
		if callee == "text" && res.Get("error.message").String() != "just a message" {
			t.Errorf("a string _call.error is its message: %s", res.Raw)
		}
	}
}

// version is the callee's active version number in the request's pinned
// snapshot, and the trace's call events carry it too.
func TestCallVersionIsTheCalleesActiveVersion(t *testing.T) {
	pu, _ := newTestUnit(t)
	db := pu.Dbc.Db
	for _, q := range []string{
		`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme', 'acme', 'Acme', '')`,
		`CREATE TABLE stacks (stack_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL, active_version INTEGER, created_at TEXT NOT NULL)`,
		`CREATE TABLE stack_versions (version_id INTEGER PRIMARY KEY, stack_id TEXT NOT NULL, version_number INTEGER NOT NULL, status TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`INSERT INTO stacks VALUES ('stk_sub', 'tnt_acme', 'sub', 42, '')`,
		`INSERT INTO stack_versions VALUES (42, 'stk_sub', 12, 'superseded', 't', '')`,
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES ('sub', 100, 's', 'EMIT ._call.output.v = true', '', '', 'tnt_acme')`,
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES ('c', 100, 'call', 'EXEC "call://sub"', '', '', 'tnt_acme')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	rec := &oracleTracer{}
	ctx := trace.WithContext(WithTenant(context.Background(), "acme"), rec)
	got, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"}}`, "c/0")
	if err != nil || len(got) != 1 {
		t.Fatalf("Run: %v %+v", err, got)
	}
	final := got[0].Raw
	if v := gjson.Get(final, "_call.version").Int(); v != 12 {
		t.Errorf("version = %d, want 12: %s", v, final)
	}
	if !gjson.Get(final, "_call.output.v").Bool() {
		t.Errorf("the tenant's callee did not run: %s", final)
	}
	var enter, ret bool
	for _, e := range rec.events {
		if strings.HasPrefix(e, "call.enter ") && strings.Contains(e, "stack=sub") && strings.Contains(e, "version=12") && strings.Contains(e, "depth=1") {
			enter = true
		}
		if strings.HasPrefix(e, "call.return ") && strings.Contains(e, "ok=true") && strings.Contains(e, "version=12") {
			ret = true
		}
	}
	if !enter || !ret {
		t.Errorf("call.enter/call.return with the version missing from the trace: %v", rec.events)
	}
}

// --- envelope isolation (§2.3, §2.5) --------------------------------------

// The callee starts from a fresh envelope: it cannot read a caller field that
// is not in input, cannot change one, and sees its own identity facts.
func TestCallEnvelopeIsolation(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "iso", 100, "peek", `WHEN .secret =~ /./ EMIT ._call.output.saw_secret = true`)
	seedOp(t, pu, "iso", 100, "facts", `EMIT ._call.output.x = @call.input.x, ._call.output.src = @src, `+
		`._call.output.depth = @call.depth, ._call.output.caller = @call.caller.op, ._call.output.stack = @stack, `+
		`.secret = "changed", .work = true`)
	seedOp(t, pu, "c", 100, "call", `WITH input = &object("x", 7) EXEC "call://iso"`)

	final := callFinal(t, pu, callerIn, "c/0")
	out := gjson.Get(final, "_call.output")
	if out.Get("saw_secret").Exists() {
		t.Errorf("the callee read a caller field not in input: %s", final)
	}
	if gjson.Get(final, "secret").String() != "s3" || gjson.Get(final, "work").Exists() {
		t.Errorf("the callee changed the caller's envelope: %s", final)
	}
	if out.Get("x").Int() != 7 || out.Get("src").String() != "call" || out.Get("depth").Int() != 1 ||
		out.Get("caller").String() != "call" || out.Get("stack").String() != "iso" {
		t.Errorf("the callee's facts: %s", out.Raw)
	}
}

// --- return (§0, §2.8) ----------------------------------------------------

// @halt in the callee ends the callee; the caller's next scope runs.
func TestCallHaltReturnsToCaller(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "h", 100, "stop", `EMIT ._call.output.h = 1, @halt = true`)
	seedOp(t, pu, "h", 200, "never", `EMIT ._call.output.never = true`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://h"`)
	seedOp(t, pu, "c", 200, "after", `EMIT .after = true`)
	final := callFinal(t, pu, callerIn, "c/0")
	if !gjson.Get(final, "_call.ok").Bool() || gjson.Get(final, "_call.output.h").Int() != 1 ||
		gjson.Get(final, "_call.output.never").Exists() || !gjson.Get(final, "after").Bool() {
		t.Errorf("halt in the callee: %s", final)
	}
}

// Nested calls return in order: a calls b calls c, and each sees its callee's
// answer before writing its own.
func TestCallNestedCallsReturnInOrder(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "a", 100, "call", `WITH into = "_b" EXEC "call://b"`)
	seedOp(t, pu, "a", 200, "own", `EMIT .order = &concat(._b.output.order, "a"), .depths = &concat(._b.output.depths, @call.depth)`)
	seedOp(t, pu, "b", 100, "call", `WITH into = "_c" EXEC "call://c"`)
	seedOp(t, pu, "b", 200, "own", `EMIT ._call.output.order = &concat(._c.output.order, "b"), ._call.output.depths = &concat(._c.output.depths, @call.depth)`)
	seedOp(t, pu, "c", 100, "own", `EMIT ._call.output.order = "c", ._call.output.depths = @call.depth`)
	final := callFinal(t, pu, `{"_txc":{"stack":"a","src":"http"}}`, "a/0")
	if got := gjson.Get(final, "order").String(); got != "cba" {
		t.Errorf("order = %q, want cba: %s", got, final)
	}
	if got := gjson.Get(final, "depths").String(); got != "21" {
		t.Errorf("depths = %q, want 21 (c at 2, b at 1; the root has none): %s", got, final)
	}
}

// Depth over the limit answers txco_call_depth. The limit can be lowered by
// configuration, never raised above 8.
func TestCallDepthLimit(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.CallDepthMax = 2
	seedOp(t, pu, "rec", 100, "again", `EXEC "call://rec"`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://rec"`)
	final := callFinal(t, pu, callerIn, "c/0")
	// rec at depth 1 calls rec at depth 2, whose own call (depth 3) is
	// refused. That refusal lands at rec/2's `_call`, which is also what the
	// engine reads back as rec/2's answer, so it is passed up unchanged.
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_depth" {
		t.Errorf("code = %q, want txco_call_depth: %s", got, final)
	}
	pu.Conf.CallDepthMax = 100
	if pu.callDepthLimit() != callDepthDefault {
		t.Errorf("the flag raised the limit to %d", pu.callDepthLimit())
	}
}

// --- fuel and transition history (§2.8) -----------------------------------

// Fuel is shared: exhaustion inside the callee ends the callee, and the
// caller's next charge ends the request on budget.
func TestCallSharesFuel(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.MaxFuelPerRequest = 60
	seedOp(t, pu, "sub", 100, "a", `EMIT .a = 1`)
	seedOp(t, pu, "sub", 150, "b", `EMIT .b = 1`)
	seedOp(t, pu, "sub", 200, "c", `EMIT ._call.output.c = 1`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://sub"`)
	seedOp(t, pu, "c", 200, "after", `EMIT .after = true`)
	final := callFinal(t, pu, callerIn, "c/0")
	if got := gjson.Get(final, "code").String(); got != "txco_fuel_exhausted" {
		t.Errorf("the request should end on fuel after the callee spent it: %s", final)
	}
	if gjson.Get(final, "after").Exists() {
		t.Errorf("the caller ran on after exhaustion: %s", final)
	}
}

// Calling the same callee twice pays no repeat-transition penalty: each
// frame starts with its own history. A tight loop inside one frame still
// pays it.
func TestCallTransitionHistoryIsPerFrame(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "sub", 100, "a", `EMIT .a = 1`)
	seedOp(t, pu, "sub", 150, "b", `EMIT ._call.output.b = 1`)
	seedOp(t, pu, "once", 100, "call", `EXEC "call://sub"`)
	seedOp(t, pu, "twice", 100, "call", `EXEC "call://sub"`)
	seedOp(t, pu, "twice", 200, "call", `WITH into = "_second" EXEC "call://sub"`)
	// The callee: one scope entered (10) and one hop (10), no dispatch (EMIT
	// only). Each call: the dispatch (25) and the scope that made it (10).
	callee := 2 * fuelCostScopeEnter
	one := fuelCostScopeEnter + fuelCostExec + callee
	if got := gjson.Get(callFinal(t, pu, `{"_txc":{"stack":"once","src":"http"}}`, "once/0"), "_txc.fuel_used").Int(); got != one {
		t.Errorf("one call: fuel %d, want %d", got, one)
	}
	final := callFinal(t, pu, `{"_txc":{"stack":"twice","src":"http"}}`, "twice/0")
	if got, want := gjson.Get(final, "_txc.fuel_used").Int(), 2*one; got != want {
		t.Errorf("two calls: fuel %d, want %d (no repeat penalty for the second): %s", got, want, final)
	}
	if !gjson.Get(final, "_second.ok").Bool() {
		t.Errorf("second call: %s", final)
	}

	// The tight loop: pp/100 -> 150 -> 100 -> 150 repeats one transition.
	seedOp(t, pu, "pp", 100, "to150", `WHEN .done != true EMIT @goto = "150"`)
	seedOp(t, pu, "pp", 150, "back", `WHEN .k2 != true EMIT .k2 = true, @goto = "100"`)
	seedOp(t, pu, "loopy", 100, "call", `EXEC "call://pp"`)
	loop := callFinal(t, pu, `{"_txc":{"stack":"loopy","src":"http"}}`, "loopy/0")
	// pp alone: four scopes entered, three jumps, one repeat (the oracle's
	// ping-pong flow records 165).
	ppAlone := 4*fuelCostScopeEnter + 3*fuelCostExec + fuelCostRepeatTransition
	if got, want := gjson.Get(loop, "_txc.fuel_used").Int(), fuelCostScopeEnter+fuelCostExec+ppAlone; got != want {
		t.Errorf("tight loop inside a call: fuel %d, want %d (the repeat still pays): %s", got, want, loop)
	}
}

// --- deadline and cancellation (§2.9) -------------------------------------

// callBlockingOp registers a txco:// op that waits for its context, or for
// `ignore` if it is to ignore cancellation; it reports how it ended.
func callBlockingOp(t *testing.T, pu *Unit, name string, ignore time.Duration) *atomic.Value {
	t.Helper()
	var ended atomic.Value
	pu.Handle([]byte("txco://"+name), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in []byte, out []byte) (event.Payload, error) {
			if ignore > 0 {
				time.Sleep(ignore)
				ended.Store("ran on")
				return event.Payload{Raw: `{"late":true}`, Type: event.JSON}, nil
			}
			select {
			case <-ctx.Done():
				ended.Store("cancelled")
				return event.Payload{Raw: `{}`, Type: event.JSON}, ctx.Err()
			case <-time.After(10 * time.Second):
				ended.Store("finished")
				return event.Payload{Raw: `{}`, Type: event.JSON}, nil
			}
		}))
	return &ended
}

// The call's own timeout bounds it, and a callee whose ops honour
// cancellation reports stopped: true.
func TestCallTimeoutStopsACooperativeCallee(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.CallStopGrace = "2s"
	ended := callBlockingOp(t, pu, "block", 0)
	seedOp(t, pu, "slow", 100, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://slow" WITH timeout = 150`)
	seedOp(t, pu, "c", 200, "after", `EMIT .after = true`)
	start := time.Now()
	final := callFinal(t, pu, callerIn, "c/0")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the call took %s; its timeout was 150 ms", took)
	}
	res := gjson.Get(final, "_call")
	if res.Get("ok").Bool() || res.Get("error.code").String() != "txco_call_timeout" {
		t.Errorf("result = %s", res.Raw)
	}
	if !res.Get("error.stopped").Bool() || res.Get("error.running").Exists() {
		t.Errorf("a cooperative callee should report stopped: true and nothing running: %s", res.Raw)
	}
	if got, _ := ended.Load().(string); got != "cancelled" {
		t.Errorf("the callee's op ended %q, want cancelled", got)
	}
	if !gjson.Get(final, "after").Bool() {
		t.Errorf("the caller did not continue: %s", final)
	}
}

// A callee can never outlive its caller's deadline.
func TestCallCannotOutliveTheCaller(t *testing.T) {
	pu, _ := newTestUnit(t)
	ended := callBlockingOp(t, pu, "block", 0)
	seedOp(t, pu, "slow", 100, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://slow" WITH timeout = 10000`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := callRun(t, pu, ctx, callerIn, "c/0")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the request took %s; its deadline was 200 ms", took)
	}
	if err == nil {
		t.Errorf("Run should report the deadline, got nil and %+v", got)
	}
	// The callee's op was cancelled with the caller, possibly a moment later.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, _ := ended.Load().(string); got == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callee's op ended %q, want cancelled", ended.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An op that ignores cancellation outlives the grace: the call reports
// stopped: false and names it.
func TestCallTimeoutReportsAnOpThatRunsOn(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.CallStopGrace = "50ms"
	ended := callBlockingOp(t, pu, "sleeper", 600*time.Millisecond)
	seedOp(t, pu, "stubborn", 100, "sleep", `EXEC "txco://sleeper" WITH timeout = 10000`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://stubborn" WITH timeout = 100`)
	final := callFinal(t, pu, callerIn, "c/0")
	res := gjson.Get(final, "_call")
	if res.Get("error.code").String() != "txco_call_timeout" || res.Get("error.stopped").Bool() {
		t.Errorf("result = %s", res.Raw)
	}
	if got := res.Get("error.running").Array(); len(got) != 1 || got[0].String() != "stubborn/100/sleep" {
		t.Errorf("running = %s, want [stubborn/100/sleep]", res.Get("error.running").Raw)
	}
	// Let it finish so the test's goroutine does not outlive the test.
	time.Sleep(700 * time.Millisecond)
	if got, _ := ended.Load().(string); got != "ran on" {
		t.Errorf("the op ended %q", got)
	}
}

// The effective deadline is the least of the three: a WITH timeout over the
// chassis maximum is clamped, not dropped.
func TestCallTimeoutOverTheMaximumIsClamped(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.OpTimeoutMax = "100ms"
	pu.Conf.CallStopGrace = "1s"
	callBlockingOp(t, pu, "block", 0)
	// Each of the callee's ops fits under the maximum on its own and ends on
	// its own timeout; together they outrun the clamped call.
	seedOp(t, pu, "slow", 100, "wait", `EXEC "txco://block" WITH timeout = 80`)
	seedOp(t, pu, "slow", 200, "wait-more", `EXEC "txco://block" WITH timeout = 80`)
	seedOp(t, pu, "slow", 300, "never", `EMIT ._call.output.finished = true`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://slow" WITH timeout = 60000`)
	start := time.Now()
	final := callFinal(t, pu, callerIn, "c/0")
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s; the chassis maximum was 100 ms", took)
	}
	if !gjson.Get(final, "_call").Exists() {
		t.Errorf("the call was dropped instead of clamped: %s", final)
	}
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_timeout" || gjson.Get(final, "_call.output.finished").Exists() {
		t.Errorf("code = %q: %s", got, final)
	}
}

// --- version-0 refusals (§2.7) --------------------------------------------

// A callee scope that would suspend — mode = continuable, mode = async with
// and without a join floor — or stream a body ends the call with
// txco_call_unsupported, creates no run record, and does so the same way on
// a chassis with and without a continuation store.
func TestCallRefusesWhatCannotReturn(t *testing.T) {
	for _, withStore := range []bool{true, false} {
		t.Run(map[bool]string{true: "with-store", false: "no-store"}[withStore], func(t *testing.T) {
			pu, _ := newTestUnit(t)
			if withStore {
				fs, err := filestore.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				pu.Runs = continuation.NewRuns(fs)
			}
			seedOp(t, pu, "cont", 100, "slow", `EXEC "http://127.0.0.1:9/x" WITH mode = "continuable", continue_after = "1s"`)
			seedOp(t, pu, "async", 100, "job", `EXEC "http://127.0.0.1:9/x" WITH mode = "async"`)
			seedOp(t, pu, "joined", 100, "job", `EXEC "http://127.0.0.1:9/x" WITH mode = "async", join_at_scope = 200`)
			seedOp(t, pu, "joined", 200, "join", `EMIT ._call.output.joined = true`)
			seedOp(t, pu, "stream", 100, "chunk", `EMIT @web.res.body = "aGk="`)
			seedOp(t, pu, "stream", 200, "more", `EMIT ._call.output.streamed = true`)
			for _, callee := range []string{"cont", "async", "joined", "stream"} {
				seedOp(t, pu, "c-"+callee, 100, "call", `EXEC "call://`+callee+`"`)
				seedOp(t, pu, "c-"+callee, 200, "after", `EMIT .after = true`)
			}
			for _, callee := range []string{"cont", "async", "joined", "stream"} {
				got, err := callRun(t, pu, context.Background(), `{"_txc":{"stack":"c-`+callee+`","src":"http"}}`, "c-"+callee+"/0")
				if err != nil || len(got) != 1 || got[0].Type != event.JSON {
					t.Fatalf("%s: %v %+v", callee, err, got)
				}
				final := got[0].Raw
				if code := gjson.Get(final, "_call.error.code").String(); code != "txco_call_unsupported" {
					t.Errorf("%s: code = %q, want txco_call_unsupported: %s", callee, code, final)
				}
				if gjson.Get(final, "_call.output").Raw != "{}" || !gjson.Get(final, "after").Bool() {
					t.Errorf("%s: %s", callee, final)
				}
			}
			if withStore {
				ids, err := pu.Runs.ListRunIDs(context.Background())
				if err != nil || len(ids) != 0 {
					t.Errorf("a refused call created run records: %v %v", ids, err)
				}
			}
		})
	}
}

// The two deferred traps: a caller with a deferred op in flight calls a
// plain callee. No 202 is sent from inside the call, and the run's result
// is the caller's, written when the caller ends — not the callee's.
func TestCallInsideACallerWithADeferredOp(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t) // acks 202 and never calls back

	seedOp(t, pu, "plain", 100, "p", `EMIT ._call.output.plain = true`)
	seedOp(t, pu, "site", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async", join_at_scope = 300`)
	seedOp(t, pu, "site", 150, "call", `EXEC "call://plain"`)
	seedOp(t, pu, "site", 200, "after", `EMIT .after = true`)
	seedOp(t, pu, "site", 300, "join", `EMIT .joined = true`)

	resCh := make(chan event.Payload, 4)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"src":"http","stack":"site"}}`, "site/100", resCh) }()
	// The request suspends at ITS join floor, after the call returned.
	rcid, _ := waitFor202(t, resCh)
	runID, err := pu.Runs.ResolveRunContinuation(context.Background(), rcid)
	if err != nil {
		t.Fatal(err)
	}
	ss, ok, err := pu.Runs.CurrentStage(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("CurrentStage: %v %v", ok, err)
	}
	if ss.Stage != "site/300" {
		t.Errorf("suspended at %q, want site/300 (a 202 from inside the call would suspend elsewhere)", ss.Stage)
	}
	if !gjson.Get(ss.ScopeEnvelope, "_call.ok").Bool() || !gjson.Get(ss.ScopeEnvelope, "_call.output.plain").Bool() ||
		!gjson.Get(ss.ScopeEnvelope, "after").Bool() {
		t.Errorf("the call did not complete before the join: %s", ss.ScopeEnvelope)
	}
	if _, has, _ := pu.Runs.ReadResult(context.Background(), runID); has {
		t.Errorf("the callee's envelope was written as the run's result")
	}
	select {
	case p := <-resCh:
		t.Errorf("a second payload reached the client: %+v", p)
	default:
	}
}

// --- the transport is untouched -------------------------------------------

// A callee's @web.res does not reach the caller's transport, and a caller
// that could stream (an HTTP request) receives one buffered answer.
func TestCallLeavesTheTransportToTheCaller(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "sub", 100, "s", `EMIT @web.res.status = 418, @web.res.body = "aGk=", ._call.output.x = 1`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://sub"`)
	seedOp(t, pu, "c", 200, "own", `EMIT @web.res.status = 201`)
	got, err := callRun(t, pu, context.Background(), callerIn, "c/0")
	if err != nil || len(got) != 1 || got[0].Type != event.JSON {
		t.Fatalf("want one JSON payload: %v %+v", err, got)
	}
	final := got[0].Raw
	if gjson.Get(final, "_txc.web.res.status").Int() != 201 || gjson.Get(final, "_txc.web.res.body").Exists() {
		t.Errorf("the callee's response fields reached the caller: %s", final)
	}
}

// --- concurrency (§2.11); run under -race ---------------------------------

func TestCallConcurrentCallsInOneScope(t *testing.T) {
	pu, _ := newTestUnit(t)
	// Each go-sqlite3 :memory: connection is its own database; the real
	// snapshot is pinned to one connection (see opsindex.go), the test
	// handle must be too for concurrent lookups.
	pu.Dbc.Db.SetMaxOpenConns(1)
	seedOp(t, pu, "sub", 100, "a", `EMIT .a = 1`)
	seedOp(t, pu, "sub", 150, "b", `EMIT ._call.output.n = @call.input.n`)
	for i := 1; i <= 5; i++ {
		n := string(rune('0' + i))
		seedOp(t, pu, "c", 100, "call"+n, `WITH input = &object("n", `+n+`), into = "_r`+n+`" EXEC "call://sub"`)
	}
	final := callFinal(t, pu, callerIn, "c/0")
	for i := 1; i <= 5; i++ {
		n := string(rune('0' + i))
		if got := gjson.Get(final, "_r"+n+".output.n").Int(); got != int64(i) {
			t.Errorf("_r%s = %s", n, gjson.Get(final, "_r"+n).Raw)
		}
	}
}

// A call inside LOOP: each pass calls again with the previous answer.
func TestCallInsideLoop(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "counter", 100, "inc", `EMIT ._call.output.n = &add(@call.input.n, 1)`)
	seedOp(t, pu, "c", 100, "count", `WITH input = &object("n", ._n) EXEC "call://counter" LOOP EVERY "1ms" SET ._n = ._call.output.n UNTIL ._call.output.n == 3 MAX 10`)
	final := callFinal(t, pu, `{"_txc":{"stack":"c","src":"http"},"_n":0}`, "c/0")
	if got := gjson.Get(final, "_call.output.n").Int(); got != 3 {
		t.Errorf("n = %d, want 3: %s", got, final)
	}
	if got := gjson.Get(final, "_txc.runtime.loop.count.passes").Int(); got != 3 {
		t.Errorf("passes = %d, want 3: %s", got, final)
	}
}

// --- what a call refuses as data, and what it refuses as authoring --------

func TestCallTargetErrors(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "c-missing", 100, "call", `EXEC "call://nowhere"`)
	seedOp(t, pu, "c-missing", 200, "after", `EMIT .after = true`)
	seedOp(t, pu, "c-with", 100, "call", `WITH stack = "bad name" EXEC "call://nowhere"`)
	seedOp(t, pu, "sub", 100, "s", `EMIT ._call.output.ok = true`)
	seedOp(t, pu, "c-picked", 100, "call", `WITH stack = .next EXEC "call://nowhere"`)

	final := callFinal(t, pu, `{"_txc":{"stack":"c-missing","src":"http"}}`, "c-missing/0")
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_no_stack" || !gjson.Get(final, "after").Bool() {
		t.Errorf("missing callee: %s", final)
	}
	final = callFinal(t, pu, `{"_txc":{"stack":"c-with","src":"http"}}`, "c-with/0")
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_target" {
		t.Errorf("bad WITH stack: %s", final)
	}
	final = callFinal(t, pu, `{"_txc":{"stack":"c-picked","src":"http"},"next":"sub"}`, "c-picked/0")
	if !gjson.Get(final, "_call.ok").Bool() || gjson.Get(final, "_call.stack").String() != "sub" {
		t.Errorf("WITH stack from data: %s", final)
	}

	// A malformed literal target is an authoring error at dispatch (the
	// parser refuses it at apply): the op is dropped, like a bad goto.
	for _, target := range []string{"call://", "call://bad name"} {
		_, transport, err := pu.Exec(context.Background(), operation.Operation{
			Resonator: &resonator.Resonator{Exec: target}, Input: `{}`,
		})
		if err == nil || !strings.Contains(err.Error(), target) || transport != "call" {
			t.Errorf("Exec(%q): err = %v transport = %q", target, err, transport)
		}
	}
}

// A chassis that is cancelled mid-call (an abort) answers txco_call_cancelled.
func TestCallCancelledAnswersAsSuch(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.CallStopGrace = "1s"
	callBlockingOp(t, pu, "block", 0)
	seedOp(t, pu, "slow", 100, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedOp(t, pu, "c", 100, "call", `EXEC "call://slow" WITH timeout = 10000`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	got, err := callRun(t, pu, ctx, callerIn, "c/0")
	if err == nil || !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("Run: %v %+v", err, got)
	}
}
