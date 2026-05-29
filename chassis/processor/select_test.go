package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
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
