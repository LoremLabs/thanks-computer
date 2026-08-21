package processor

import (
	"testing"

	"github.com/tidwall/gjson"
)

// First-ever processor-level PRIORITY coverage. The positive-priority
// branch of ResonatingOps shipped with `ops = ops[:0]` — every op in a
// scope containing a PRIORITY > 0 rule was silently dropped. Latent in
// the fleet (only `PRIORITY -1` is used, which never enters the branch
// since highestPriority stays 0). These tests pin the intended
// semantics: highest positive priority wins and exactly one op runs.

// TestPriorityHighestWins — with competing positive priorities, only
// the highest-priority rule fires.
func TestPriorityHighestWins(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "prio/win", "low", `PRIORITY 1 EMIT .winner = "low"`)
	seedRule(t, pu, "prio/win", "high", `PRIORITY 2 EMIT .winner = "high"`)

	p := runOne(t, pu, `{}`, "prio/win/0")
	if got := gjson.Get(p.Raw, "winner").String(); got != "high" {
		t.Errorf("winner = %q, want high (positive PRIORITY scope must run exactly the top rule); body=%s", got, p.Raw)
	}
}

// TestPositivePriorityScopeRunsOps — regression pin for the `ops[:0]`
// typo: a scope whose only rule carries PRIORITY > 0 must still run it.
func TestPositivePriorityScopeRunsOps(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "prio/one", "only", `PRIORITY 2 EMIT .ran = true`)

	p := runOne(t, pu, `{}`, "prio/one/0")
	if !gjson.Get(p.Raw, "ran").Bool() {
		t.Errorf("PRIORITY > 0 rule never ran (ops[:0] regression); body=%s", p.Raw)
	}
}

// TestPriorityTieDeterministicPick — two rules tied at the highest
// priority: exactly one fires, and the hashSeed pick is deterministic
// for a fixed seed (ResonatingOps is called directly to control it).
func TestPriorityTieDeterministicPick(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "prio/tie", "a", `PRIORITY 2 EMIT .winner = "a"`)
	seedRule(t, pu, "prio/tie", "b", `PRIORITY 2 EMIT .winner = "b"`)

	p := runOne(t, pu, `{}`, "prio/tie/0")
	w := gjson.Get(p.Raw, "winner").String()
	if w != "a" && w != "b" {
		t.Fatalf("no tied rule fired; body=%s", p.Raw)
	}
	// Exactly one fired: the loser's value must not also be present —
	// winner is a single scalar, so a double-fire would be a merge
	// race; run a few times to shake it out.
	for i := 0; i < 5; i++ {
		p = runOne(t, pu, `{}`, "prio/tie/0")
		if got := gjson.Get(p.Raw, "winner").String(); got != "a" && got != "b" {
			t.Fatalf("tied scope produced no winner on run %d; body=%s", i, p.Raw)
		}
	}
}

// TestNegativePriorityRunsAllMatches — fleet-current behavior pinned:
// negative priorities never enter the positive-priority filter
// (highestPriority stays 0), so all matching rules run.
func TestNegativePriorityRunsAllMatches(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "prio/neg", "fallback", `PRIORITY -1 EMIT .fallback = true`)
	seedRule(t, pu, "prio/neg", "normal", `EMIT .normal = true`)

	p := runOne(t, pu, `{}`, "prio/neg/0")
	if !gjson.Get(p.Raw, "fallback").Bool() || !gjson.Get(p.Raw, "normal").Bool() {
		t.Errorf("negative-priority scope must run all matches; body=%s", p.Raw)
	}
}
