package resonator

import "testing"

// TestCompileMatch: a parsed regex leaf carries its compiled pattern, so
// evaluation never compiles; a hand-built one (no CompileMatch) still works
// through the bounded cache; an invalid pattern is false either way.
func TestCompileMatch(t *testing.T) {
	c := Condition{Branch: &Branch{Path: ".s"}, MatchType: "=~", MatchValue: "^ab"}
	c.CompileMatch()
	if c.re == nil {
		t.Fatal("CompileMatch left the pattern uncompiled")
	}
	if !evalLeaf(&c, `{"s":"abc"}`) || evalLeaf(&c, `{"s":"xabc"}`) {
		t.Fatal("compiled =~ evaluates wrongly")
	}

	hand := Condition{Branch: &Branch{Path: ".s"}, MatchType: "!~", MatchValue: "^ab"}
	if evalLeaf(&hand, `{"s":"abc"}`) || !evalLeaf(&hand, `{"s":"xabc"}`) {
		t.Fatal("hand-built !~ evaluates wrongly")
	}

	bad := Condition{Branch: &Branch{Path: ".s"}, MatchType: "=~", MatchValue: "(unclosed"}
	bad.CompileMatch()
	if bad.re != nil || evalLeaf(&bad, `{"s":"(unclosed"}`) {
		t.Fatal("an invalid pattern must stay uncompiled and evaluate false")
	}
	notBad := bad
	notBad.MatchType = "!~"
	if evalLeaf(&notBad, `{"s":"x"}`) {
		t.Fatal("an invalid pattern is false for !~ too")
	}

	eq := Condition{Branch: &Branch{Path: ".s"}, MatchType: "eq", MatchValue: "x"}
	eq.CompileMatch()
	if eq.re != nil {
		t.Fatal("CompileMatch must ignore non-regex comparisons")
	}
}
