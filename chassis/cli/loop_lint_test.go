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
