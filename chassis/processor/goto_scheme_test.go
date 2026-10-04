package processor

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// runJump seeds one jumping rule at origin/0, a rule at origin/1 that must be
// skipped, a landing rule at target/100 and one at origin/7, runs from
// origin/0 and returns the final envelope.
func runJump(t *testing.T, jump string) string {
	t.Helper()
	pu, _ := newTestUnit(t)
	seed := func(stack string, scope int, name, txcl string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			stack, scope, name, txcl); err != nil {
			t.Fatalf("seed %s/%d: %v", stack, scope, err)
		}
	}
	seed("origin", 0, "jump", jump)
	seed("origin", 1, "skipped", `EMIT .skipped = true`)
	seed("origin", 7, "here", `EMIT .landed = "origin/7"`)
	seed("target", 100, "there", `EMIT .landed = "target/100"`)

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"_txc":{"stack":"origin"}}`, "origin/0", resCh); err != nil {
		t.Fatalf("Run(%s): %v", jump, err)
	}
	select {
	case p := <-resCh:
		return p.Raw
	default:
		t.Fatalf("Run(%s): no response", jump)
		return ""
	}
}

// `EXEC "goto://…"` behaves exactly as `EMIT @goto` does: the same envelope
// comes out, fuel included, for a stage in another stack and for a bare scope
// in this one. (A rule with no EXEC that writes @goto pays the EXEC dispatch
// fuel for its jump, so every spelling of a jump costs the same.)
func TestGotoSchemeIsEmitGoto(t *testing.T) {
	for _, tc := range []struct {
		name, emit, exec, landed string
	}{
		{"another stack", `EMIT @goto = "target/100"`, `EXEC "goto://target/100"`, "target/100"},
		{"a scope in this stack", `EMIT @goto = "7"`, `EXEC "goto://7"`, "origin/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viaEmit := runJump(t, tc.emit)
			viaGoto := runJump(t, tc.exec)
			if viaGoto != viaEmit {
				t.Errorf("goto:// and @goto differ:\n  @goto:  %s\n  goto:// %s", viaEmit, viaGoto)
			}
			if got := gjson.Get(viaGoto, "landed").String(); got != tc.landed {
				t.Errorf("landed at %q, want %q: %s", got, tc.landed, viaGoto)
			}
			if gjson.Get(viaGoto, "skipped").Exists() {
				t.Errorf("origin/1 ran; the jump should have skipped it: %s", viaGoto)
			}
			if gjson.Get(viaGoto, "_txc.goto").Exists() {
				t.Errorf("_txc.goto leaked into the response: %s", viaGoto)
			}
			// The stage moved; the run's stack identity did not.
			if got := gjson.Get(viaGoto, "_txc.stack").String(); got != "origin" {
				t.Errorf("_txc.stack = %q, want it to stay %q: %s", got, "origin", viaGoto)
			}
		})
	}
}

// A jump costs one EXEC dispatch however it is written, and only one: a rule
// that has an EXEC of its own and also writes @goto pays its EXEC, not two.
func TestGotoFuelIsOneDispatch(t *testing.T) {
	fuel := func(jump string) int64 { return gjson.Get(runJump(t, jump), "_txc.fuel_used").Int() }
	// Two scopes entered (origin/0, then the target), plus one dispatch.
	want := 2*fuelCostScopeEnter + fuelCostExec
	for _, jump := range []string{
		`EMIT @goto = "target/100"`,
		`SELECT * SET @goto = "target/100"`, // a SET after SELECT writes the answer
		`EXEC "goto://target/100"`,
		`EXEC "target/100"`,
		`SELECT * SET @goto = "target/100" EXEC "goto://target/100"`, // has an EXEC: pays it once
	} {
		if got := fuel(jump); got != want {
			t.Errorf("%s: fuel %d, want %d (two scopes and one dispatch)", jump, got, want)
		}
	}
	// A rule with no EXEC that does not jump pays nothing to dispatch: one
	// writing other fields, and a SET before the SELECT, which decorates the
	// op's input and so never jumps. Both run on through origin/1 and origin/7.
	for _, rule := range []string{`EMIT .note = "no jump"`, `SET @goto = "target/100"`} {
		raw := runJump(t, rule)
		if got, want := gjson.Get(raw, "_txc.fuel_used").Int(), 3*fuelCostScopeEnter; got != want {
			t.Errorf("%s: fuel %d, want %d (three scopes, no dispatch): %s", rule, got, want, raw)
		}
	}
}

// The unschemed form lands in the same place too.
func TestUnschemedStageJumpMatchesGotoScheme(t *testing.T) {
	if a, b := runJump(t, `EXEC "target/100"`), runJump(t, `EXEC "goto://target/100"`); a != b {
		t.Errorf("unschemed and goto:// differ:\n  unschemed: %s\n  goto://    %s", a, b)
	}
}

// A bad target fails loudly at dispatch (the parser refuses it at apply time,
// so this is the belt to that brace).
func TestExecGotoSchemeBadTarget(t *testing.T) {
	pu, _ := newTestUnit(t)
	for _, target := range []string{"goto://", "goto://billing", "goto://bad name/1"} {
		_, transport, err := pu.Exec(context.Background(), operation.Operation{
			Resonator: &resonator.Resonator{Exec: target},
			Input:     `{}`,
		})
		if err == nil || !strings.Contains(err.Error(), target) {
			t.Errorf("Exec(%q): err = %v, want an error naming the EXEC", target, err)
		}
		if transport != "unsupported" {
			t.Errorf("Exec(%q): transport = %q, want unsupported (untrusted)", target, transport)
		}
	}
}

// A stage jump's payload carries its target as a JSON value. The goto
// transport is trusted and merges raw, so a quote in a target must not be
// able to add fields beside `goto`.
func TestStageJumpPayloadEscapesTarget(t *testing.T) {
	target := `x","halt":true,"tenant":"other","y":"/1`
	p := stageJumpPayload(target)
	if !gjson.Valid(p.Raw) {
		t.Fatalf("payload is not JSON: %s", p.Raw)
	}
	if got := gjson.Get(p.Raw, "_txc.goto").String(); got != target {
		t.Errorf("_txc.goto = %q, want the target verbatim", got)
	}
	if n := len(gjson.Get(p.Raw, "_txc").Map()); n != 1 {
		t.Errorf("_txc has %d fields, want only goto: %s", n, p.Raw)
	}
}
