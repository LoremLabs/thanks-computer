package processor

import (
	"regexp"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
)

// TestOverlayResponseOverwriteSemantics locks in EMIT's runtime
// behavior: unlike DecorateInput (set-if-absent, used by SET PRE /
// SET POST), OverlayResponse always overwrites. EMIT is the
// "contribute literal fields to the merge" primitive.
func TestOverlayResponseOverwriteSemantics(t *testing.T) {
	pu, _ := newTestUnit(t)

	cases := []struct {
		name      string
		in        string
		overrides []resonator.BranchValue
		// checks are gjson path -> expected JSON-stringified value.
		checks map[string]string
	}{
		{
			name: "empty input is treated as empty object",
			in:   "",
			overrides: []resonator.BranchValue{
				{Path: ".tag", Value: ast.Literal{V: "cruel"}},
			},
			checks: map[string]string{"tag": `"cruel"`},
		},
		{
			name: "writes a new top-level field",
			in:   `{"foo":"bar"}`,
			overrides: []resonator.BranchValue{
				{Path: ".words", Value: ast.Literal{V: []interface{}{"cruel"}}},
			},
			checks: map[string]string{
				"foo":   `"bar"`,
				"words": `["cruel"]`,
			},
		},
		{
			name: "OVERWRITES an existing field (set-not-set-if-absent)",
			in:   `{"tag":"hello"}`,
			overrides: []resonator.BranchValue{
				{Path: ".tag", Value: ast.Literal{V: "cruel"}},
			},
			checks: map[string]string{"tag": `"cruel"`},
		},
		{
			name: "multiple overrides applied in order",
			in:   `{}`,
			overrides: []resonator.BranchValue{
				{Path: ".a", Value: ast.Literal{V: int64(1)}},
				{Path: ".b", Value: ast.Literal{V: float64(2.5)}},
				{Path: ".c", Value: ast.Literal{V: true}},
				{Path: ".d", Value: ast.Literal{V: []interface{}{int64(1), int64(2), int64(3)}}},
			},
			checks: map[string]string{
				"a": "1",
				"b": "2.5",
				"c": "true",
				"d": "[1,2,3]",
			},
		},
		{
			name: "nested path through dot syntax",
			in:   `{"meta":{"existing":1}}`,
			overrides: []resonator.BranchValue{
				{Path: ".meta.added", Value: ast.Literal{V: "yes"}},
			},
			checks: map[string]string{
				"meta.existing": "1",
				"meta.added":    `"yes"`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-PR-2/4 callers passed `in` once and expected it to
			// serve as both the path-resolution env AND the write
			// target. With the env split, tests that don't use
			// PathRef-on-RHS work identically by passing `in` twice
			// (env=in, output=in) — literal-only EMITs don't read the
			// env at all, so equivalence holds.
			got, err := pu.OverlayResponse(tc.in, tc.in, tc.overrides)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for path, want := range tc.checks {
				gotVal := gjson.Get(got, path).Raw
				if gotVal != want {
					t.Errorf("path %q: got %q, want %q (full body: %s)", path, gotVal, want, got)
				}
			}
		})
	}
}

// TestOverlayResponseEMITReadsFromExecOutput is the regression that
// pins what `EMIT @reply = .text` is supposed to do: project a value
// from the EXEC's fresh response (`output`) into the response payload.
// Before the MultiEnv fix, OverlayResponse used only `env` (op.Input)
// for resolution, so any `.text` against an op whose handler put `text`
// in its output (e.g. ai://chat) resolved to nil — the documented
// "pair with EXEC for enrich the response" pattern silently broke.
//
// Composition: a function-call value (`&b64encode(.text)`) tightens
// the regression: the inner path must resolve to a real string for
// the function to succeed. nil → error from &b64encode.
func TestOverlayResponseEMITReadsFromExecOutput(t *testing.T) {
	pu, _ := newTestUnit(t)

	// op.Input — scope-entry envelope. Has @web.req fields (under _txc)
	// but no `text` at root.
	envIn := `{"_txc":{"web":{"req":{"body":"aGVsbG8="}}}}`

	// op.Output — what the EXEC produced (a chat handler's response).
	output := `{"text":"Soup dumplings are joy."}`

	// EMIT @reply = .text  — should project the EXEC's text to the
	// response. With the fix, .text resolves against output and the
	// overlay writes _txc.reply = "Soup dumplings are joy.".
	// (Parser strips leading dot from PathRef.Path — see parser.go:237.)
	overrides := []resonator.BranchValue{
		{Path: "._txc.reply", Value: ast.PathRef{Path: "text"}},
	}

	got, err := pu.OverlayResponse(envIn, output, overrides)
	if err != nil {
		t.Fatalf("OverlayResponse: %v", err)
	}
	if g := gjson.Get(got, "_txc.reply").String(); g != "Soup dumplings are joy." {
		t.Errorf("_txc.reply = %q, want %q (raw=%s)", g, "Soup dumplings are joy.", got)
	}

	// Tighter: wrap .text in &b64encode. The inner path must still
	// resolve to the real string for the function to succeed.
	overrides2 := []resonator.BranchValue{
		{Path: "._txc.web.res.body", Value: ast.FunctionCall{
			Name: "b64encode",
			Args: []ast.Value{ast.PathRef{Path: "text"}},
		}},
	}
	got2, err := pu.OverlayResponse(envIn, output, overrides2)
	if err != nil {
		t.Fatalf("OverlayResponse with b64encode: %v", err)
	}
	// base64 of "Soup dumplings are joy." is U291cCBkdW1wbGluZ3MgYXJlIGpveS4=
	if g := gjson.Get(got2, "_txc.web.res.body").String(); g != "U291cCBkdW1wbGluZ3MgYXJlIGpveS4=" {
		t.Errorf("_txc.web.res.body = %q, want b64 of the text (raw=%s)", g, got2)
	}
}

// TestOverlayResponseEMITStillReadsFromInput pins the other half of
// the composition: input-envelope reads (`@web.req.body` → `_txc.web.
// req.body`) still work when the EXEC output doesn't have that path.
// Composition matters — the MultiEnv fix puts output FIRST but must
// fall through to input.
func TestOverlayResponseEMITStillReadsFromInput(t *testing.T) {
	pu, _ := newTestUnit(t)

	envIn := `{"_txc":{"web":{"req":{"body":"aGVsbG8="}}}}`
	output := `{"text":"unrelated"}`

	overrides := []resonator.BranchValue{
		{Path: "._txc.copy", Value: ast.PathRef{Path: "_txc.web.req.body"}},
	}
	got, err := pu.OverlayResponse(envIn, output, overrides)
	if err != nil {
		t.Fatalf("OverlayResponse: %v", err)
	}
	if g := gjson.Get(got, "_txc.copy").String(); g != "aGVsbG8=" {
		t.Errorf("_txc.copy = %q, want %q (raw=%s)", g, "aGVsbG8=", got)
	}
}

// TestOverlayResponseEMITOutputPrecedence pins the precedence rule
// when the SAME path exists in both envs: the EXEC output wins. This
// is the "fresh data is what the rule means by now" semantic.
func TestOverlayResponseEMITOutputPrecedence(t *testing.T) {
	pu, _ := newTestUnit(t)

	envIn := `{"text":"old-from-input"}`
	output := `{"text":"new-from-output"}`

	overrides := []resonator.BranchValue{
		{Path: "._txc.chosen", Value: ast.PathRef{Path: "text"}},
	}
	got, err := pu.OverlayResponse(envIn, output, overrides)
	if err != nil {
		t.Fatalf("OverlayResponse: %v", err)
	}
	if g := gjson.Get(got, "_txc.chosen").String(); g != "new-from-output" {
		t.Errorf("expected output to win precedence; got %q (raw=%s)", g, got)
	}
}

// TestOverlayResponse_ResolveError exercises the strict-halt path
// that fires when runtime.Resolve returns an error. Easiest trigger
// is a FunctionCall to an unregistered name, which surfaces from
// funcs.Call as "unknown function &…". The test pins the contract:
// a resolution failure halts the overlay loop and returns
// (partially-applied input, error).
func TestOverlayResponse_ResolveError(t *testing.T) {
	pu, _ := newTestUnit(t)

	overrides := []resonator.BranchValue{
		// First override is fine; second triggers the not-yet-supported
		// FunctionCall path so we can verify it short-circuits.
		{Path: ".ok", Value: ast.Literal{V: "stays"}},
		{Path: ".bad", Value: ast.FunctionCall{Name: "definitely-not-a-real-function"}},
		{Path: ".never", Value: ast.Literal{V: "unreached"}},
	}
	in := `{"keep":"yes"}`

	got, err := pu.OverlayResponse(in, in, overrides)
	if err == nil {
		t.Fatalf("expected error, got nil; result %s", got)
	}
	// First override applied (overlay processes in order); second halted.
	// `.never` must NOT appear — the third override never ran.
	if gjson.Get(got, "ok").String() != "stays" {
		t.Errorf("expected first override applied; got body %s", got)
	}
	if gjson.Get(got, "never").Exists() {
		t.Errorf("expected halt before third override; got body %s", got)
	}
}

// TestDecorateInput_ResolveError pins the same strict-halt contract
// on the SET PRE / SET POST code path.
func TestDecorateInput_ResolveError(t *testing.T) {
	pu, _ := newTestUnit(t)

	overrides := []resonator.BranchValue{
		{Path: ".ok", Value: ast.Literal{V: "stays"}},
		{Path: ".bad", Value: ast.FunctionCall{Name: "definitely-not-a-real-function"}},
		{Path: ".never", Value: ast.Literal{V: "unreached"}},
	}
	in := `{"keep":"yes"}`

	got, err := pu.DecorateInput(in, overrides)
	if err == nil {
		t.Fatalf("expected error, got nil; result %s", got)
	}
	if gjson.Get(got, "ok").String() != "stays" {
		t.Errorf("expected first override applied; got body %s", got)
	}
	if gjson.Get(got, "never").Exists() {
		t.Errorf("expected halt before third override; got body %s", got)
	}
}

// uuidV7Pattern matches the standard 8-4-4-4-12 hex layout with the
// v7 version nibble (7) and the variant bits (8, 9, a, or b). The
// processor smoke tests below assert results match this shape — a
// proxy for "the parse → resonator → runtime → funcs.uuid pipeline
// actually wired up end-to-end."
var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestE2E_FunctionCallThroughSetPre proves the full PR-3 pipeline:
// a rule string containing `&uuid()` parses into an ast.FunctionCall
// node, lands in resonator.SetPre, and when DecorateInput runs it the
// runtime evaluator dispatches to funcs.uuidFn and the resulting v7
// UUID string lands in the envelope.
func TestE2E_FunctionCallThroughSetPre(t *testing.T) {
	pu, _ := newTestUnit(t)

	src := `SET .id = &uuid()`
	res := parser.New(lexer.New(src)).ParseEvent()
	if res == nil || res.SetPre == nil {
		t.Fatalf("parse produced no SET PRE; got %#v", res)
	}

	got, err := pu.DecorateInput(`{}`, res.SetPre.Overrides)
	if err != nil {
		t.Fatalf("DecorateInput failed: %v", err)
	}
	id := gjson.Get(got, "id").String()
	if !uuidV7Pattern.MatchString(id) {
		t.Errorf("id %q is not a v7 UUID (body: %s)", id, got)
	}
}

// TestE2E_FunctionCallThroughEmit proves the same plumbing on the
// OverlayResponse (EMIT) path. Same parser front, different
// envelope-writer at the back.
func TestE2E_FunctionCallThroughEmit(t *testing.T) {
	pu, _ := newTestUnit(t)

	src := `EMIT .id = &uuid()`
	res := parser.New(lexer.New(src)).ParseEvent()
	if res == nil || res.Emit == nil {
		t.Fatalf("parse produced no EMIT; got %#v", res)
	}

	got, err := pu.OverlayResponse(`{}`, `{}`, res.Emit.Overrides)
	if err != nil {
		t.Fatalf("OverlayResponse failed: %v", err)
	}
	id := gjson.Get(got, "id").String()
	if !uuidV7Pattern.MatchString(id) {
		t.Errorf("id %q is not a v7 UUID (body: %s)", id, got)
	}
}

// TestOverlayResponse_PathRefResolvesAgainstEnv pins the env-vs-output
// split (the fix for the live-chassis EMIT bug uncovered in PR 6
// verification). An EMIT whose RHS is a PathRef must resolve that path
// against the ENV (the envelope the op saw at dispatch), not against
// the EMIT's local write accumulator. If the two ever get conflated,
// PathRefs in EMIT come back nil because the accumulator starts empty.
func TestOverlayResponse_PathRefResolvesAgainstEnv(t *testing.T) {
	pu, _ := newTestUnit(t)

	// env has the source value at `_txc.web.req.body`.
	env := `{"_txc":{"web":{"req":{"body":"aGVsbG8="}}}}`
	// output (the EMIT accumulator) starts empty — the value is NOT
	// in here. If OverlayResponse mistakenly resolved against output,
	// the PathRef would come back nil and &b64decode would error.
	output := `{}`

	// Parse a real rule so the RHS becomes an ast.FunctionCall whose
	// arg is an ast.PathRef — same path the live MCP server uses.
	rule := `EMIT .decoded = &b64decode(@web.req.body)`
	res := parser.New(lexer.New(rule)).ParseEvent()
	if res == nil || res.Emit == nil {
		t.Fatalf("parse produced no EMIT; got %#v", res)
	}

	got, err := pu.OverlayResponse(env, output, res.Emit.Overrides)
	if err != nil {
		t.Fatalf("OverlayResponse should succeed (env carries the path), got err: %v", err)
	}
	if v := gjson.Get(got, "decoded").String(); v != "hello" {
		t.Errorf("expected 'hello', got %q (output: %s)", v, got)
	}

	// Negative case: same rule but the path source isn't in env.
	// PathRef resolves to nil → &b64decode rejects → error propagates.
	emptyEnv := `{}`
	_, err = pu.OverlayResponse(emptyEnv, output, res.Emit.Overrides)
	if err == nil {
		t.Errorf("expected error when env lacks the path; got success")
	}
}

// TestE2E_NowFunctionWithFormat exercises a one-arg call end-to-end:
// `SET .ts = &now("rfc3339")` should produce a parseable RFC 3339
// timestamp in the envelope.
func TestE2E_NowFunctionWithFormat(t *testing.T) {
	pu, _ := newTestUnit(t)

	src := `SET .ts = &now("rfc3339")`
	res := parser.New(lexer.New(src)).ParseEvent()
	if res == nil || res.SetPre == nil {
		t.Fatalf("parse produced no SET PRE; got %#v", res)
	}

	got, err := pu.DecorateInput(`{}`, res.SetPre.Overrides)
	if err != nil {
		t.Fatalf("DecorateInput failed: %v", err)
	}
	ts := gjson.Get(got, "ts").String()
	// Loose shape check: RFC 3339 always has a 'T' between date and time.
	if len(ts) < 19 || ts[10] != 'T' {
		t.Errorf("ts %q is not RFC 3339-shaped (body: %s)", ts, got)
	}
}
