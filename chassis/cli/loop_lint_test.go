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

// --- LOOP clause lint ---

func loopOp(txcl string) []bundle.Op {
	return []bundle.Op{{
		Stack: "loop", Scope: 0, Name: "pager",
		SourcePath: "/tmp/pager.txcl",
		Txcl:       txcl,
	}}
}

func TestLintLoopCleanLoopIsSilent(t *testing.T) {
	for _, txcl := range []string{
		`WITH after = ._p.next EXEC "txco://blob/list" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 50`,
		`EXEC "https://api.example.com/jobs/42" LOOP UNTIL .status == "done"`,
		`EXEC "https://api.example.com/items" LOOP SET .cursor = ._items.next UNTIL ._items.next == "" MAX 1000`,
	} {
		if got := lintLoopClause(loopOp(txcl)); len(got) != 0 {
			t.Errorf("%s: expected no warnings, got: %v", txcl, got)
		}
	}
}

func TestLintLoopPolarityTrap(t *testing.T) {
	traps := []string{
		`EXEC "txco://x" LOOP UNTIL ._p.more == false`,
		`EXEC "txco://x" LOOP UNTIL ._p.more != true`,
		`EXEC "txco://x" LOOP UNTIL !(._p.more == true)`,
		`EXEC "txco://x" LOOP UNTIL ._p.next == "" || ._p.more == false`,
	}
	for _, txcl := range traps {
		if got := lintLoopClause(loopOp(txcl)); !containsSubstring(got, "exits after the first pass") {
			t.Errorf("%s: expected polarity warning, got: %v", txcl, got)
		}
	}
	safe := []string{
		`EXEC "txco://x" LOOP UNTIL ._p.done == true`,
		`EXEC "txco://x" LOOP UNTIL ._p.more != false`,
		`EXEC "txco://x" LOOP UNTIL !(._p.more != true)`,
		`EXEC "txco://x" LOOP UNTIL ._p.next == ""`,
	}
	for _, txcl := range safe {
		if got := lintLoopClause(loopOp(txcl)); containsSubstring(got, "exits after the first pass") {
			t.Errorf("%s: unexpected polarity warning: %v", txcl, got)
		}
	}
}

func TestLintLoopHeldBackSchemes(t *testing.T) {
	for _, txcl := range []string{
		`EXEC "compute://sha256/abc" LOOP UNTIL .done == true`,
		`EXEC "ai://chat" LOOP UNTIL ._draft.done == true`,
	} {
		if got := lintLoopClause(loopOp(txcl)); !containsSubstring(got, "not admitted") {
			t.Errorf("%s: expected not-admitted warning, got: %v", txcl, got)
		}
	}
	if got := lintLoopClause(loopOp(`EXEC "txco://mock" LOOP UNTIL .done == true`)); !containsSubstring(got, "fixture") {
		t.Errorf("expected mock warning, got: %v", got)
	}
}

func TestLintLoopMaxOverDefaultCeiling(t *testing.T) {
	got := lintLoopClause(loopOp(`EXEC "txco://x" LOOP UNTIL .done == true MAX 5000`))
	if !containsSubstring(got, "exceeds the default --op-loop-max") {
		t.Errorf("expected ceiling warning, got: %v", got)
	}
}

func TestLintLoopUnconditionalHaltSibling(t *testing.T) {
	ops := []bundle.Op{
		{Stack: "loop", Scope: 0, Name: "pager", SourcePath: "/tmp/pager.txcl",
			Txcl: `EXEC "txco://x" LOOP UNTIL .done == true`},
		{Stack: "loop", Scope: 0, Name: "gate", SourcePath: "/tmp/gate.txcl",
			Txcl: `EMIT @halt = true`},
	}
	if got := lintLoopClause(ops); !containsSubstring(got, "halts unconditionally") {
		t.Errorf("expected halt-sibling warning, got: %v", got)
	}
	// A guarded halt, or one at a later scope, is fine.
	ops[1].Txcl = `WHEN .denied == true EMIT @halt = true`
	if got := lintLoopClause(ops); containsSubstring(got, "halts unconditionally") {
		t.Errorf("guarded halt should not be flagged: %v", got)
	}
	ops[1].Txcl = `EMIT @halt = true`
	ops[1].Scope = 100
	if got := lintLoopClause(ops); containsSubstring(got, "halts unconditionally") {
		t.Errorf("later-scope halt should not be flagged: %v", got)
	}
}

// TestLintLoopIgnoresOrdinaryOps: the fixtures the loop lint already
// accepts stay silent under the LOOP lint too.
func TestLintLoopIgnoresOrdinaryOps(t *testing.T) {
	ops := []bundle.Op{
		{Stack: "boot", Scope: 0, Name: "poll", SourcePath: "/tmp/poll.txcl", Txcl: `WHEN .ready != true EMIT @goto = "boot/0"`},
		{Stack: "boot", Scope: 0, Name: "http", SourcePath: "/tmp/http.txcl", Txcl: `WITH timeout = 1000, mode = "async", max = 3 EXEC "http://example.com/api/0"`},
		{Stack: "boot", Scope: 0, Name: "once", SourcePath: "/tmp/once.txcl", Txcl: `EMIT @goto = "boot/0", @halt = true`},
	}
	if got := lintLoopClause(ops); len(got) != 0 {
		t.Errorf("expected no warnings for ordinary ops, got: %v", got)
	}
}
