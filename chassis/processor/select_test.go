package processor

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// seedRule inserts a single ops row into the per-test SQLite handle so a
// Run(...) at stack/0 has something to dispatch. Mirrors the pattern in
// processor_mock_test.go.
func seedRule(t *testing.T, pu *Unit, stack, name, rule string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		stack, 0, name, rule); err != nil {
		t.Fatalf("seed op (%s/%s): %v", stack, name, err)
	}
}

func runOne(t *testing.T, pu *Unit, in, stage string) event.Payload {
	t.Helper()
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), in, stage, resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case p := <-resCh:
		return p
	default:
		t.Fatal("no response received")
		return event.Payload{}
	}
}

// TestSelectCopiesPathNoExec — `SELECT @x AS .y` without an EXEC copies
// the source value to the destination and the write persists into the
// merged scope response (the synthetic EMIT carries it past the noop).
func TestSelectCopiesPathNoExec(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/copy", "copy", `SELECT .source AS .dest`)

	p := runOne(t, pu, `{"source":"hello"}`, "sel/copy/0")
	if got := gjson.Get(p.Raw, "dest").String(); got != "hello" {
		t.Errorf("dest = %q, want hello; body=%s", got, p.Raw)
	}
}

// TestSelectUsesDefaultOnMissingSource — `DEFAULT` substitutes when the
// source path is absent from the envelope.
func TestSelectUsesDefaultOnMissingSource(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/dflt", "dflt", `SELECT .absent AS .out DEFAULT "fallback"`)

	p := runOne(t, pu, `{}`, "sel/dflt/0")
	if got := gjson.Get(p.Raw, "out").String(); got != "fallback" {
		t.Errorf("out = %q, want fallback; body=%s", got, p.Raw)
	}
}

// TestSelectUsesDefaultOnEmptyString — source resolves to "" → default
// kicks in. Matches the `txco://copy` op's behavior so authors can
// migrate one for one.
func TestSelectUsesDefaultOnEmptyString(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/empty", "empty", `SELECT .src AS .out DEFAULT "fallback"`)

	p := runOne(t, pu, `{"src":""}`, "sel/empty/0")
	if got := gjson.Get(p.Raw, "out").String(); got != "fallback" {
		t.Errorf("empty-string source: out = %q, want fallback", got)
	}
}

// TestSelectDefaultSkippedWhenSourcePresent — DEFAULT must NOT clobber
// a populated source.
func TestSelectDefaultSkippedWhenSourcePresent(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/skip", "skip", `SELECT .src AS .out DEFAULT "fallback"`)

	p := runOne(t, pu, `{"src":"real"}`, "sel/skip/0")
	if got := gjson.Get(p.Raw, "out").String(); got != "real" {
		t.Errorf("present source overridden by default: got %q", got)
	}
}

// TestSelectAtSugar — `@x` resolves to `_txc.x`, same convention WHEN
// and WITH use. Validates the lexer→processor pipeline end-to-end.
func TestSelectAtSugar(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/at", "at", `SELECT @inner.value AS .out`)

	p := runOne(t, pu, `{"_txc":{"inner":{"value":"deep"}}}`, "sel/at/0")
	if got := gjson.Get(p.Raw, "out").String(); got != "deep" {
		t.Errorf("@-path copy: out = %q, want deep; body=%s", got, p.Raw)
	}
}

// TestSelectCopiesStructuredValue — non-string sources (objects,
// arrays, numbers) preserve their JSON shape via SetRaw under the
// hood. Without this, copy would str-coerce.
func TestSelectCopiesStructuredValue(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/struct", "struct", `SELECT .data AS .cloned`)

	p := runOne(t, pu, `{"data":{"x":1,"y":[1,2]}}`, "sel/struct/0")
	if got := gjson.Get(p.Raw, "cloned.x").Int(); got != 1 {
		t.Errorf("cloned.x = %d, want 1", got)
	}
	arr := gjson.Get(p.Raw, "cloned.y").Array()
	if len(arr) != 2 || arr[0].Int() != 1 || arr[1].Int() != 2 {
		t.Errorf("cloned.y = %v, want [1,2]", arr)
	}
}

// TestSelectMultipleAssignments — multiple `, AS, DEFAULT` clauses
// in one SELECT statement.
func TestSelectMultipleAssignments(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/multi", "multi",
		`SELECT .a AS .x, .missing AS .y DEFAULT "fallback"`)

	p := runOne(t, pu, `{"a":"first"}`, "sel/multi/0")
	if got := gjson.Get(p.Raw, "x").String(); got != "first" {
		t.Errorf("x = %q, want first", got)
	}
	if got := gjson.Get(p.Raw, "y").String(); got != "fallback" {
		t.Errorf("y = %q, want fallback", got)
	}
}

// TestSelectVisibleToExec — when a rule has SELECT + EXEC, the EXEC
// sees the SELECT'd values on its input view. Mounts a tiny test
// handler that captures its input and returns it as the response;
// asserts the SELECT'd path is present on the way in.
func TestSelectVisibleToExec(t *testing.T) {
	pu, _ := newTestUnit(t)

	// Mount a `txco://test-select-passthrough` handler that returns
	// its input verbatim. Lets us assert the SELECT'd path reached
	// the handler — bare processor test unit doesn't register
	// general-purpose passthrough ops by default.
	pu.Handle([]byte("txco://test-select-passthrough"),
		event.OpsHandlerFunc(func(_ context.Context, _ string, in, _ []byte) (event.Payload, error) {
			return event.Payload{Raw: string(in), Type: event.JSON}, nil
		}))

	seedRule(t, pu, "sel/exec", "exec",
		`SELECT .a AS .b EXEC "txco://test-select-passthrough"`)

	p := runOne(t, pu, `{"a":"seen"}`, "sel/exec/0")
	if got := gjson.Get(p.Raw, "b").String(); got != "seen" {
		t.Errorf("EXEC didn't see SELECT'd path; b = %q, want seen; body=%s", got, p.Raw)
	}
}

// TestSelectWhenCondition — SELECT only runs when the WHEN gate
// matches; absent input → rule doesn't fire → no .out written.
func TestSelectWhenCondition(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/when", "when",
		`WHEN .gate == "go" SELECT .a AS .out`)

	// Gate fails — SELECT doesn't fire.
	p := runOne(t, pu, `{"a":"x","gate":"stop"}`, "sel/when/0")
	if got := gjson.Get(p.Raw, "out"); got.Exists() {
		t.Errorf("SELECT fired with failed gate; out = %v", got)
	}

	// Gate passes — SELECT writes.
	p = runOne(t, pu, `{"a":"x","gate":"go"}`, "sel/when/0")
	if got := gjson.Get(p.Raw, "out").String(); got != "x" {
		t.Errorf("SELECT didn't fire with passing gate; out = %q", got)
	}
}

// TestSelectDefaultTypedNumber — DEFAULT preserves the literal's Go
// type (here: int64), so downstream gjson `.Int()` reads back cleanly.
func TestSelectDefaultTypedNumber(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/numdflt", "numdflt",
		`SELECT .absent AS .out DEFAULT 42`)

	p := runOne(t, pu, `{}`, "sel/numdflt/0")
	if got := gjson.Get(p.Raw, "out").Int(); got != 42 {
		t.Errorf("numeric default: out = %d, want 42", got)
	}
}

// TestSelectEachRequestCarriesItsOwnValue — the parsed rule is shared by
// every request that runs it (opsindex: "read-only by contract"), so
// SELECT's per-request writes must never land on it. They used to: the
// synthetic EMIT entries were prepended onto the shared Resonator.Emit,
// so from the second request on the merged response carried the FIRST
// request's value at every SELECT'd path (later entries win, and the
// first request's were always last). Three requests, three values.
func TestSelectEachRequestCarriesItsOwnValue(t *testing.T) {
	cases := []struct {
		name, rule string
		tag        bool // the rule's own EMIT writes .tag = "t"
	}{
		{"no exec", `SELECT .v AS .out`, false},
		{"exec", `SELECT .v AS .out EXEC "txco://test-capture"`, false},
		{"with emit", `SELECT .v AS .out EMIT .tag = "t"`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pu, _ := newTestUnit(t)
			got := captureHandler(t, pu, "txco://test-capture")
			seedRule(t, pu, "sel/each", "each", tc.rule)

			for _, v := range []string{"seqA", "seqB", "seqC"} {
				p := runOne(t, pu, `{"v":"`+v+`"}`, "sel/each/0")
				if out := gjson.Get(p.Raw, "out").String(); out != v {
					t.Errorf("request v=%s: merged out = %q, want %q; body=%s", v, out, v, p.Raw)
				}
				if tc.tag && gjson.Get(p.Raw, "tag").String() != "t" {
					t.Errorf("request v=%s: the rule's own EMIT lost; body=%s", v, p.Raw)
				}
				if c, _ := got.Load().(string); c != "" && gjson.Get(c, "out").String() != v {
					t.Errorf("request v=%s: op was handed out = %q", v, gjson.Get(c, "out").String())
				}
			}
		})
	}
}

// TestSelectAuthoredEmitStillWins — a rule's own EMIT on a SELECT'd path
// overrides the SELECT on every request, not just the first.
func TestSelectAuthoredEmitStillWins(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "sel/pin", "pin", `SELECT .v AS .out EMIT .out = "pinned"`)

	for _, v := range []string{"a", "b", "c"} {
		p := runOne(t, pu, `{"v":"`+v+`"}`, "sel/pin/0")
		if out := gjson.Get(p.Raw, "out").String(); out != "pinned" {
			t.Errorf("request v=%s: out = %q, want pinned; body=%s", v, out, p.Raw)
		}
	}
}

// TestSelectLeavesSharedRuleUntouched — ResonatingOps hands back an op
// whose Emit carries this request's SELECT values, and the parsed rule
// it came from is exactly as it was: no Emit grown onto a rule that had
// none, no entries added to one that had some.
func TestSelectLeavesSharedRuleUntouched(t *testing.T) {
	pu, _ := newTestUnit(t)
	ctx := context.Background()
	seedRule(t, pu, "sel/bare", "bare", `SELECT .v AS .out`)
	seedRule(t, pu, "sel/emit", "emit", `SELECT .v AS .out EMIT .tag = "t"`)

	for _, stage := range []string{"sel/bare/0", "sel/emit/0"} {
		ops, err := pu.OpsForStage(ctx, stage)
		if err != nil || len(ops) != 1 {
			t.Fatalf("%s: OpsForStage = %d ops, %v", stage, len(ops), err)
		}
		if ops[0].Resonator == nil {
			res, perr := txcl.Resonator(ops[0].Txcl)
			if perr != nil {
				t.Fatalf("%s: parse: %v", stage, perr)
			}
			ops[0].Resonator = res
		}
		shared := ops[0].Resonator
		before := 0
		if shared.Emit != nil {
			before = len(shared.Emit.Overrides)
		}
		for _, v := range []string{"a", "b"} {
			in := make([]operation.Operation, len(ops))
			copy(in, ops)
			got, err := pu.ResonatingOps(`{"v":"`+v+`"}`, in, "")
			if err != nil || len(got) != 1 {
				t.Fatalf("%s: ResonatingOps = %d ops, %v", stage, len(got), err)
			}
			if got[0].Resonator == shared {
				t.Errorf("%s: the SELECT'd op still points at the shared rule", stage)
			}
			if e := got[0].Resonator.Emit; e == nil || len(e.Overrides) != before+1 || e.Overrides[0].Path != ".out" {
				t.Errorf("%s: this op's Emit = %+v, want the SELECT entry then the rule's %d", stage, e, before)
			}
		}
		after := 0
		if shared.Emit != nil {
			after = len(shared.Emit.Overrides)
		}
		if stage == "sel/bare/0" && shared.Emit != nil {
			t.Errorf("%s: the shared rule grew an Emit: %+v", stage, shared.Emit)
		}
		if after != before {
			t.Errorf("%s: shared rule's Emit went from %d to %d entries", stage, before, after)
		}
	}
}

// TestSelectConcurrentRequestsKeepTheirOwnValues — many requests at once
// through one parsed rule; each merged response carries its own value.
// Run with -race: appending onto the shared rule was also a data race.
func TestSelectConcurrentRequestsKeepTheirOwnValues(t *testing.T) {
	pu, _ := newTestUnit(t)
	// One connection: each go-sqlite3 :memory: connection is its own
	// empty database (dbcache pins the real snapshot the same way).
	pu.Dbc.Db.SetMaxOpenConns(1)
	seedRule(t, pu, "sel/many", "many", `SELECT .v AS .out EMIT .tag = "t"`)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				v := fmt.Sprintf("g%d-i%d", g, i)
				resCh := make(chan event.Payload, 1)
				if err := pu.Run(context.Background(), `{"v":"`+v+`"}`, "sel/many/0", resCh); err != nil {
					t.Errorf("Run: %v", err)
					return
				}
				p := <-resCh
				if out := gjson.Get(p.Raw, "out").String(); out != v {
					t.Errorf("request %s: out = %q", v, out)
				}
			}
		}(g)
	}
	wg.Wait()
}
