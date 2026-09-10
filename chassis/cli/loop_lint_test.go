package cli

import (
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
)

// TestLintFlagsUnconditionalSelfLoop asserts the lint surfaces an obvious
// typo: an EMIT @goto pointing back at the rule's own (stack, scope) with
// no WHEN guard and no terminating @halt.
func TestLintFlagsUnconditionalSelfLoop(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack:      "boot",
			Scope:      0,
			Name:       "loop",
			SourcePath: "/tmp/loop.txcl",
			Txcl:       `EMIT @goto = "boot/0"`,
		},
	}
	got := lintStackLoops(ops)
	if !containsSubstring(got, "unconditionally emits @goto back to its own stage") {
		t.Errorf("expected self-loop warning, got: %v", got)
	}
}

// TestLintFlagsUnconditionalSelfExec asserts the unschemed EXEC
// stage-jump syntax also trips the self-loop detector.
func TestLintFlagsUnconditionalSelfExec(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack:      "boot",
			Scope:      0,
			Name:       "loop",
			SourcePath: "/tmp/loop.txcl",
			Txcl:       `EXEC "boot/0"`,
		},
	}
	got := lintStackLoops(ops)
	if !containsSubstring(got, "unconditionally EXECs into its own stage") {
		t.Errorf("expected self-EXEC warning, got: %v", got)
	}
}

// TestLintFlagsTwoStackPingPong constructs A→B and B→A unconditional
// EMITs and asserts the cycle is reported exactly once (deduped by the
// canonical pair key).
func TestLintFlagsTwoStackPingPong(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "a", Scope: 0, Name: "to-b",
			SourcePath: "/tmp/a.txcl",
			Txcl:       `EMIT @goto = "b/0"`,
		},
		{
			Stack: "b", Scope: 0, Name: "to-a",
			SourcePath: "/tmp/b.txcl",
			Txcl:       `EMIT @goto = "a/0"`,
		},
	}
	got := lintStackLoops(ops)

	cycleCount := 0
	for _, w := range got {
		if strings.Contains(w, "2-stack cycle") {
			cycleCount++
		}
	}
	if cycleCount != 1 {
		t.Errorf("expected exactly 1 cycle warning, got %d (warnings: %v)", cycleCount, got)
	}
}

// TestLintAllowsConditionalLoop verifies that a guarded self-loop — the
// canonical polling idiom — is NOT flagged. The WHEN clause makes the
// loop conditional; the runtime budget guards bound iteration count.
func TestLintAllowsConditionalLoop(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "boot", Scope: 0, Name: "poll",
			SourcePath: "/tmp/poll.txcl",
			Txcl:       `WHEN .ready != true EMIT @goto = "boot/0"`,
		},
	}
	got := lintStackLoops(ops)
	if len(got) != 0 {
		t.Errorf("expected no warnings for guarded loop, got: %v", got)
	}
}

// TestLintAllowsHaltingLoop verifies that a rule emitting both @goto and
// @halt=true is not flagged. The halt terminates the pipeline before the
// goto can fire repeatedly.
func TestLintAllowsHaltingLoop(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "boot", Scope: 0, Name: "once",
			SourcePath: "/tmp/once.txcl",
			Txcl:       `EMIT @goto = "boot/0", @halt = true`,
		},
	}
	got := lintStackLoops(ops)
	if len(got) != 0 {
		t.Errorf("expected no warnings for halt+goto, got: %v", got)
	}
}

// TestLintIgnoresSchemedExec verifies that an EXEC with a URL scheme
// (http://, txco://) is not classified as a stage jump even if its path
// happens to end in /<digits>.
func TestLintIgnoresSchemedExec(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "boot", Scope: 0, Name: "http",
			SourcePath: "/tmp/http.txcl",
			Txcl:       `EXEC "http://example.com/api/0"`,
		},
	}
	got := lintStackLoops(ops)
	if len(got) != 0 {
		t.Errorf("expected no warnings for HTTP EXEC, got: %v", got)
	}
}

func containsSubstring(warnings []string, want string) bool {
	for _, w := range warnings {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}

// TestLintFlagsCrossStackGoto asserts a `@goto` literal naming another stack
// is surfaced. The rule is GUARDED — that is the point: a real dispatch always
// sits behind a WHEN, so the loop lint's "skip guarded rules" rule would miss
// every true positive here.
func TestLintFlagsCrossStackGoto(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack:      "core",
			Scope:      630,
			Name:       "dispatch",
			SourcePath: "/tmp/dispatch.txcl",
			Txcl:       `WHEN .x == true` + "\n" + `  EMIT @goto = "kind-triage/2010"`,
		},
	}
	got := lintCrossStackGoto(ops)
	if !containsSubstring(got, "does not re-pin _txc.stack") {
		t.Errorf("expected cross-stack goto warning, got: %v", got)
	}
	if !containsSubstring(got, "txco://route") {
		t.Errorf("warning should name the fix, got: %v", got)
	}
}

// TestLintIgnoresSameStackGoto — the common case. A jump within the same stack
// needs no re-pin, so it must stay silent or the lint is noise. Covers both the
// qualified and bare forms, since resolveStageRef reads a bare scope as
// "current stack".
func TestLintIgnoresSameStackGoto(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "core", Scope: 680, Name: "loop", SourcePath: "/tmp/a.txcl",
			Txcl: `WHEN .x == true` + "\n" + `  EMIT @goto = "core/580"`,
		},
		{
			Stack: "core", Scope: 690, Name: "bare", SourcePath: "/tmp/b.txcl",
			Txcl: `WHEN .y == true` + "\n" + `  EMIT @goto = "580"`,
		},
	}
	if got := lintCrossStackGoto(ops); len(got) != 0 {
		t.Errorf("same-stack goto must not warn, got: %v", got)
	}
}

// TestLintIgnoresPathValuedGoto — `@goto = ._ret` is the subroutine-return
// idiom and its target is not knowable statically, so it must not warn.
func TestLintIgnoresPathValuedGoto(t *testing.T) {
	ops := []bundle.Op{
		{
			Stack: "kind-triage", Scope: 2011, Name: "ret", SourcePath: "/tmp/ret.txcl",
			Txcl: `WHEN .x == true` + "\n" + `  EMIT @goto = ._ret.stage`,
		},
	}
	if got := lintCrossStackGoto(ops); len(got) != 0 {
		t.Errorf("path-valued goto must not warn, got: %v", got)
	}
}

// --- WITH repeat_until / repeat_max lint ---

func repeatOp(txcl string) []bundle.Op {
	return []bundle.Op{{
		Stack: "loop", Scope: 0, Name: "pager",
		SourcePath: "/tmp/pager.txcl",
		Txcl:       txcl,
	}}
}

func TestLintRepeatCleanLoopIsSilent(t *testing.T) {
	got := lintRepeatDirectives(repeatOp(
		`WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 50 EXEC "txco://blob/list"`))
	if len(got) != 0 {
		t.Errorf("expected no warnings for a well-formed loop, got: %v", got)
	}
}

func TestLintRepeatMissingMax(t *testing.T) {
	got := lintRepeatDirectives(repeatOp(
		`WITH repeat_until = ._p.next == "" EXEC "txco://blob/list"`))
	if !containsSubstring(got, "without repeat_max") {
		t.Errorf("expected missing repeat_max warning, got: %v", got)
	}
}

func TestLintRepeatMaxNotPositiveLiteral(t *testing.T) {
	for _, txcl := range []string{
		`WITH repeat_until = ._p.next == "", repeat_max = 0 EXEC "txco://blob/list"`,
		`WITH repeat_until = ._p.next == "", repeat_max = ._n EXEC "txco://blob/list"`,
		`WITH repeat_until = ._p.next == "", repeat_max = "50" EXEC "txco://blob/list"`,
	} {
		got := lintRepeatDirectives(repeatOp(txcl))
		if !containsSubstring(got, "positive integer literal") {
			t.Errorf("%s: expected repeat_max literal warning, got: %v", txcl, got)
		}
	}
}

func TestLintRepeatMaxWithoutUntil(t *testing.T) {
	got := lintRepeatDirectives(repeatOp(`WITH repeat_max = 5 EXEC "txco://blob/list"`))
	if !containsSubstring(got, "repeat_max without repeat_until") {
		t.Errorf("expected ignored repeat_max warning, got: %v", got)
	}
}

func TestLintRepeatNonTxcoExec(t *testing.T) {
	for _, txcl := range []string{
		`WITH repeat_until = ._p.next == "", repeat_max = 5 EXEC "https://example.com/page"`,
		`WITH repeat_until = ._p.next == "", repeat_max = 5`,
	} {
		got := lintRepeatDirectives(repeatOp(txcl))
		if !containsSubstring(got, "repeats txco:// ops only") {
			t.Errorf("%s: expected non-txco warning, got: %v", txcl, got)
		}
	}
}

func TestLintRepeatPolarityTrap(t *testing.T) {
	traps := []string{
		`WITH repeat_until = ._p.more == false, repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = ._p.more != true, repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = !(._p.more == true), repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = ._p.next == "" || ._p.more == false, repeat_max = 5 EXEC "txco://x"`,
	}
	for _, txcl := range traps {
		got := lintRepeatDirectives(repeatOp(txcl))
		if !containsSubstring(got, "exits after the first pass") {
			t.Errorf("%s: expected polarity warning, got: %v", txcl, got)
		}
	}
	safe := []string{
		`WITH repeat_until = ._p.done == true, repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = ._p.more != false, repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = !(._p.more != true), repeat_max = 5 EXEC "txco://x"`,
		`WITH repeat_until = ._p.next == "", repeat_max = 5 EXEC "txco://x"`,
	}
	for _, txcl := range safe {
		got := lintRepeatDirectives(repeatOp(txcl))
		if containsSubstring(got, "exits after the first pass") {
			t.Errorf("%s: unexpected polarity warning: %v", txcl, got)
		}
	}
}

func TestLintRepeatStackedWithSelfGoto(t *testing.T) {
	got := lintRepeatDirectives(repeatOp(
		`WITH repeat_until = ._p.next == "", repeat_max = 5 EXEC "txco://x" EMIT @goto = "loop/0"`))
	if !containsSubstring(got, "two loops stacked") {
		t.Errorf("expected stacked-loop warning, got: %v", got)
	}
	guarded := lintRepeatDirectives(repeatOp(
		`WHEN .again == true WITH repeat_until = ._p.next == "", repeat_max = 5 EXEC "txco://x" EMIT @goto = "loop/0"`))
	if containsSubstring(guarded, "two loops stacked") {
		t.Errorf("guarded goto should not be flagged: %v", guarded)
	}
}

func TestLintRepeatBudgetReserved(t *testing.T) {
	for _, txcl := range []string{
		`WITH repeat_until = ._p.next == "", repeat_max = 5, repeat_budget = "5s" EXEC "txco://x"`,
		`WITH repeat_budget = "5s" EXEC "txco://x"`,
	} {
		got := lintRepeatDirectives(repeatOp(txcl))
		if !containsSubstring(got, "repeat_budget, which is reserved") {
			t.Errorf("%s: expected reserved warning, got: %v", txcl, got)
		}
	}
}

// TestLintRepeatIgnoresOrdinaryOps: the fixtures the loop lint already
// accepts stay silent under the repeat lint too.
func TestLintRepeatIgnoresOrdinaryOps(t *testing.T) {
	ops := []bundle.Op{
		{Stack: "boot", Scope: 0, Name: "poll", SourcePath: "/tmp/poll.txcl", Txcl: `WHEN .ready != true EMIT @goto = "boot/0"`},
		{Stack: "boot", Scope: 0, Name: "http", SourcePath: "/tmp/http.txcl", Txcl: `WITH timeout = 1000, mode = "async" EXEC "http://example.com/api/0"`},
		{Stack: "boot", Scope: 0, Name: "once", SourcePath: "/tmp/once.txcl", Txcl: `EMIT @goto = "boot/0", @halt = true`},
	}
	if got := lintRepeatDirectives(ops); len(got) != 0 {
		t.Errorf("expected no warnings for ordinary ops, got: %v", got)
	}
}
