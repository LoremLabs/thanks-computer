package processor

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcguard"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/runtime"
)

// TestAuthorPathCannotPadAnArray — sjson reads an all-digit path key as an
// array index and pads with `null` out to it, so an author-supplied path is
// also a size: `EMIT .a.2000000 = 1` was a 10 MB envelope from a 33-byte rule
// (and a bigger index, gigabytes). Every way a rule can name a path is bounded
// by txcguard.BoundedSet*; the run completes with the write dropped.
func TestAuthorPathCannotPadAnArray(t *testing.T) {
	const envelope = `{"x":1,"claim":"v","holder":{},"_txc":{"src":"http"}}`
	rules := []struct{ name, rule string }{
		{"EMIT", `WHEN .x == 1 EMIT .a.2000000 = 1`},
		{"EMIT nested", `WHEN .x == 1 EMIT .a.b.c.2000000.d = 1`},
		{"EMIT _txc", `WHEN .x == 1 EMIT @web.res.headers.x.2000000 = "v"`},
		{"SELECT AS", `WHEN .x == 1 SELECT .claim AS .a.2000000`},
		{"copy to", `WHEN .x == 1 EXEC "txco://copy" WITH from = ".claim", to = ".a.2000000"`},
		{"overflow", `WHEN .x == 1 EMIT .a.99999999999999999999999 = 1`},
	}
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			got := runOneRule(t, "pad", r.rule, envelope)
			if len(got) > 4096 {
				t.Errorf("envelope grew to %d bytes — an array was padded", len(got))
			}
			if gjson.Get(got, "x").Int() != 1 {
				t.Errorf("the run did not complete normally: %.300s", got)
			}
		})
	}
}

// TestNumericKeysThatAreNotPaddingStillWork — the bound refuses only a write
// that would pad. A numeric OBJECT key, a `:`-forced key, a small index and
// an append all behave exactly as before.
func TestNumericKeysThatAreNotPaddingStillWork(t *testing.T) {
	const envelope = `{"x":1,"holder":{"by_id":{}},"_txc":{"src":"http"}}`
	cases := []struct{ name, rule, probe, want string }{
		// `by_id` exists and is an object, so the digits are an object key.
		{"object key under an existing object",
			`WHEN .x == 1 EMIT .out = &set(.holder, "by_id.1690000000", "u")`, `out.by_id.1690000000`, "u"},
		{"forced object key", `WHEN .x == 1 EMIT .out = &set(.holder, "fresh.:1690000000", "u")`, `out.fresh.1690000000`, "u"},
		{"small index", `WHEN .x == 1 EMIT .rows.3 = "r"`, "rows.3", "r"},
		{"response header slot", `WHEN .x == 1 EMIT @web.res.headers.x-a.0 = "h"`, "_txc.web.res.headers.x-a.0", "h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runOneRule(t, "pad", c.rule, envelope)
			if v := gjson.Get(got, c.probe).String(); v != c.want {
				t.Errorf("%s = %q, want %q: %.300s", c.probe, v, c.want, got)
			}
		})
	}
}

// TestDigitNamedOpIsAnObjectKey — `2000000` is a valid op name, and the
// chassis keys its own bookkeeping by op name (`_txc.runtime.loop.<op>`). That
// key is forced to an object key, so a digit-named op cannot pad an array
// either.
func TestDigitNamedOpIsAnObjectKey(t *testing.T) {
	rule := `WHEN .x == 1 EXEC "txco://copy" WITH from = ".claim", to = ".copied" LOOP EVERY "2ms" UNTIL .never == true MAX 2`
	got := runNamedRule(t, "pad", "2000000", rule, `{"x":1,"claim":"v","_txc":{"src":"http"}}`)
	if len(got) > 4096 {
		t.Fatalf("envelope grew to %d bytes — the op name was read as an array index", len(got))
	}
	if gjson.Get(got, "_txc.runtime.loop").IsArray() {
		t.Fatalf("_txc.runtime.loop became an array: %.200s", got)
	}
	if stop := gjson.Get(got, `_txc.runtime.loop.2000000.stop`).String(); stop != "max" {
		t.Errorf("loop bookkeeping not at _txc.runtime.loop.2000000 (stop=%q): %s", stop, strings.TrimSpace(got))
	}
}

// TestSetClausesAreBounded — SET shapes an op's transient input, so its
// padding never reaches the propagated envelope where the test above could
// see it: DecorateInput is checked directly. A refused or malformed path also
// leaves the input as it was (it used to blank it).
func TestSetClausesAreBounded(t *testing.T) {
	pu, _ := newTestUnit(t)
	const input = `{"x":1}`
	for _, path := range []string{".a.2000000", ".a.99999999999999999999999", ".a.*.b"} {
		got, err := pu.DecorateInput(input, []resonator.BranchValue{{Path: path, Value: ast.Literal{V: 1}}})
		if err != nil {
			t.Fatalf("SET %s: %v", path, err)
		}
		if got != input {
			t.Errorf("SET %s: input = %.80q (%d bytes), want it unchanged", path, got, len(got))
		}
	}
	got, _ := pu.DecorateInput(input, []resonator.BranchValue{{Path: ".rows.3", Value: ast.Literal{V: "r"}}})
	if gjson.Get(got, "rows.3").String() != "r" {
		t.Errorf("an ordinary SET was refused: %s", got)
	}
}

// TestWithKeyIsBounded — a WITH key is a path into the op's params
// (`WITH secrets.key.secret = …`), so it is bounded too, and fails the clause
// rather than running the op with mangled params.
func TestWithKeyIsBounded(t *testing.T) {
	pu, _ := newTestUnit(t)
	res := &resonator.Resonator{With: map[string]ast.Value{"a.2000000": ast.Literal{V: 1}}}
	meta, err := pu.resolveWith(res, runtime.JSONEnv(`{}`))
	if !errors.Is(err, txcguard.ErrArrayPad) {
		t.Errorf("resolveWith = %.80q, %v; want ErrArrayPad", meta, err)
	}
	if len(meta) > 64 {
		t.Errorf("meta grew to %d bytes", len(meta))
	}
	res = &resonator.Resonator{With: map[string]ast.Value{"secrets.key.secret": ast.Literal{V: "K"}, "retry.0": ast.Literal{V: 1}}}
	if meta, err = pu.resolveWith(res, runtime.JSONEnv(`{}`)); err != nil || gjson.Get(meta, "secrets.key.secret").String() != "K" {
		t.Errorf("an ordinary WITH was refused: %q, %v", meta, err)
	}
}

// TestDynamicPathWritesAreBounded is a tripwire, not a proof. A JSON write
// whose PATH is a bare variable is the shape that lets an author-supplied path
// size the document (see txcguard/pad.go). Such a write must go through
// txcguard.BoundedSet*, unless the variable is one of the op-target names that
// the guarded resolvers produce (intoPath / boundedInto / authorTarget /
// computedTarget already refuse a padding target). A literal path, or one
// built from a literal prefix, is the chassis's own and is not flagged.
func TestDynamicPathWritesAreBounded(t *testing.T) {
	bareVar := regexp.MustCompile(`sjson\.Set(?:Raw)?(?:Bytes)?\(\s*[^,()]+,\s*([A-Za-z_][A-Za-z0-9_]*)\s*,`)
	guarded := map[string]bool{
		"into": true, "to": true, "outputPath": true, "configuredPath": true, // resolved by a guarded helper
		"opDebugField": true, // a const
	}
	exempt := map[string]bool{
		"redact.go:p": true, // writes only where gjson already found a value: replaces in place
	}
	for _, dir := range []string{".", "../server", "../ops", "../txcl/funcs", "../trace", "../server/llmgw"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			base := filepath.Base(f)
			if strings.HasSuffix(base, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range bareVar.FindAllStringSubmatch(string(src), -1) {
				if guarded[m[1]] || exempt[base+":"+m[1]] {
					continue
				}
				t.Errorf("%s: %s… writes at a variable path %q — use txcguard.BoundedSet* (a numeric key in an author-supplied path pads an array out to its index)", f, strings.TrimSpace(m[0]), m[1])
			}
		}
	}
}
