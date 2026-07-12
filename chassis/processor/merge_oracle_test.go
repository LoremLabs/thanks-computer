package processor

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// The fast path must be indistinguishable from the sjson-based slow
// path: same bytes, same error. These tests drive both with generated
// inputs designed to hit every gate (case collisions, duplicate keys,
// gjson-special key chars, whitespace docs, merge-error shapes) as
// well as the plain envelope traffic the fast path exists for.

type mergeGen struct {
	r    *rand.Rand
	keys []string
}

var mergeGenPlainKeys = []string{
	"alpha", "beta", "gamma", "_txc", "count", "a", "b", "name",
	"zoo", "foo", "x/y", "$id", "user name", "k-1", "v:2", "a.b",
	"123", "-1", "_files", "status",
}

var mergeGenKeys = []string{
	// plain (fast-path eligible)
	"alpha", "beta", "gamma", "_txc", "count", "a", "b", "name",
	"zoo", "foo", "x/y", "$id", "user name", "k-1", "v:2", "a.b",
	"123", "-1", "_files", "status",
	// nasty (bail-triggering: case collisions with plain pool,
	// wildcards, escapes, unicode, empty)
	"Alpha", "ALPHA", "Name", "key*", "wild?", "arr#", "pipe|x",
	"back\\slash", "emoji\U0001F388", "asd\nf", "", "@mod", "sp[0]",
}

var mergeGenStrings = []string{
	"", "plain", "with space", `quote"inside`, "back\\slash",
	"line\nbreak", "tab\tchar", "<html>&stuff</html>",
	"unicode é世界", "emoji \U0001F513", "ctrlbyte",
	"ends}brace", "1e21",
}

var mergeGenNumbers = []string{
	"0", "1", "47", "-1.11", "0.5", "1e21", "-0", "3.14159",
	"123456789012345678901234567890", "2.5e-10", "1E+3", "-7",
}

func (g *mergeGen) rawString() string {
	b, _ := json.Marshal(mergeGenStrings[g.r.Intn(len(mergeGenStrings))])
	return string(b)
}

func (g *mergeGen) rawValue(depth int) string {
	n := g.r.Intn(10)
	if depth > 2 {
		n = g.r.Intn(6) // scalars only
	}
	switch n {
	case 0, 1:
		return g.rawString()
	case 2, 3:
		return mergeGenNumbers[g.r.Intn(len(mergeGenNumbers))]
	case 4:
		if g.r.Intn(2) == 0 {
			return "true"
		}
		return "false"
	case 5:
		return "null"
	case 6, 7:
		// object
		var sb strings.Builder
		sb.WriteByte('{')
		kn := g.r.Intn(4)
		for i := 0; i < kn; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			kb, _ := json.Marshal(g.keys[g.r.Intn(len(g.keys))])
			sb.Write(kb)
			sb.WriteByte(':')
			sb.WriteString(g.rawValue(depth + 1))
		}
		sb.WriteByte('}')
		return sb.String()
	default:
		// array
		var sb strings.Builder
		sb.WriteByte('[')
		kn := g.r.Intn(4)
		for i := 0; i < kn; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(g.rawValue(depth + 1))
		}
		sb.WriteByte(']')
		return sb.String()
	}
}

// doc builds a top-level document: usually an object (with optional
// duplicate keys and whitespace), sometimes a non-object to exercise
// the error path.
func (g *mergeGen) doc() string {
	switch g.r.Intn(20) {
	case 0:
		return ""
	case 1:
		return "[1,2,3]"
	case 2:
		return `"not an object"`
	case 3:
		return "42"
	}
	pretty := g.r.Intn(5) == 0
	sep, colon, open, close_ := ",", ":", "{", "}"
	if pretty {
		sep, colon, open, close_ = ",\n  ", ": ", "{\n  ", "\n}"
	}
	var sb strings.Builder
	sb.WriteString(open)
	kn := g.r.Intn(8)
	var used []string
	for i := 0; i < kn; i++ {
		if i > 0 {
			sb.WriteString(sep)
		}
		var k string
		if len(used) > 0 && g.r.Intn(12) == 0 {
			k = used[g.r.Intn(len(used))] // duplicate key
		} else {
			k = g.keys[g.r.Intn(len(g.keys))]
		}
		used = append(used, k)
		kb, _ := json.Marshal(k)
		sb.Write(kb)
		sb.WriteString(colon)
		sb.WriteString(g.rawValue(0))
	}
	if kn == 0 && pretty {
		return "{ }"
	}
	sb.WriteString(close_)
	return sb.String()
}

func errsMatch(a, b error) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Error() == b.Error()
}

func TestMergeJSONFastMatchesSlow(t *testing.T) {
	pools := []struct {
		name    string
		keys    []string
		minRate float64
	}{
		// adversarial pool: most docs contain a bail trigger; the
		// point is gate correctness, not hit rate
		{"nasty", mergeGenKeys, 0},
		// plain pool: realistic envelope keys; dense differential
		// coverage of the splice logic itself. The floor is a
		// tripwire, not a target: same-key type conflicts (dst
		// object over src scalar) legitimately bail because the
		// slow path errors there.
		{"plain", mergeGenPlainKeys, 0.15},
	}
	pu := &Unit{}
	for _, pool := range pools {
		t.Run(pool.name, func(t *testing.T) {
			g := &mergeGen{r: rand.New(rand.NewSource(20260712)), keys: pool.keys}
			fastHits := 0
			total := 0
			for i := 0; i < 20000; i++ {
				src := g.doc()
				dst := g.doc()
				slowOut, slowErr := pu.mergeJSONSlow(src, dst)
				gotOut, gotErr := pu.MergeJSON(src, dst)
				if gotOut != slowOut || !errsMatch(gotErr, slowErr) {
					t.Fatalf("iter %d mismatch\nsrc:  %q\ndst:  %q\nslow: %q (err %v)\ngot:  %q (err %v)",
						i, src, dst, slowOut, slowErr, gotOut, gotErr)
				}
				if _, ok := mergeJSONFast(src, dst); ok {
					fastHits++
				}
				total++
			}
			rate := float64(fastHits) / float64(total)
			t.Logf("fast path hit rate: %d/%d (%.1f%%)", fastHits, total, 100*rate)
			if rate <= pool.minRate {
				t.Fatalf("fast path hit rate %.1f%% below floor %.1f%% — gate regression",
					100*rate, 100*pool.minRate)
			}
		})
	}
}

// TestMergeJSONFastCoverage pins the fast path ON for representative
// envelope traffic so a future gate change can't silently route the
// hot path to the slow merge.
func TestMergeJSONFastCoverage(t *testing.T) {
	cases := []struct{ name, src, dst string }{
		{"envelope+op-output", benchSmallEnvelope(), `{"found":true,"status":"ok","count":42}`},
		{"empty+object", `{}`, `{"a":true}`},
		{"deepmerge", `{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false],"bar":{"zap":"os"}}}`, `{"zoo":["b"],"foo":{"thing":true,"baz":[3]}}`},
		{"files-accumulate", `{"_files":{"a":{"found":true}}}`, `{"_files":{"b":{"found":true,"content":"QUJD"}}}`},
		{"txc-admission", `{"_txc":{"web":{"req":{"host":"x"}}}}`, `{"_txc":{"admission":{"denied":true,"status":429}}}`},
	}
	for _, c := range cases {
		if _, ok := mergeJSONFast(c.src, c.dst); !ok {
			t.Errorf("%s: fast path bailed on representative input", c.name)
		}
	}
}

func FuzzMergeJSON(f *testing.F) {
	seeds := [][2]string{
		{`{}`, `{"a":true}`},
		{`{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false],"bar":{"zap":"os"}}}`, `{"zoo":["b"],"foo":{"thing":true,"baz":[3]}}`},
		{`{"address":"214 harvard street"}`, `{"name":{"first":"Janet","last":"Prichard"},"asdf":"hrm","age":47}`},
		{`{"key":1}`, `{"Key":{"a":2}}`},
		{`{"a":1, "b":2}`, `{"a":{"c":3}}`},
		{`{ }`, `{"x":"1e21"}`},
		{`{"dup":1,"dup":2}`, `{"dup":3}`},
		{`{"outter":{"inner":"second"}}`, `{"outter":{"inner":{"inner2":"first"}}}`},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	pu := &Unit{}
	f.Fuzz(func(t *testing.T, src, dst string) {
		// Only assert when the fast path engages: on bail, MergeJSON
		// *is* mergeJSONSlow, so equality is true by construction and
		// there is nothing to test. This also keeps the fuzzer out of
		// a pre-existing slow-path pathology on adversarial garbage:
		// an empty-string key poisons the doc to "" (sjson path
		// error), after which a huge numeric key like
		// "0010000000000000" makes sjson null-pad an empty array with
		// ~10^13 elements — an OOM, not a merge bug the fast path
		// could reproduce. Fast-accepted inputs (plain keys, valid
		// objects) provably cannot reach that code path.
		fastOut, ok := mergeJSONFast(src, dst)
		if !ok {
			return
		}
		slowOut, slowErr := pu.mergeJSONSlow(src, dst)
		if slowErr != nil || fastOut != slowOut {
			t.Fatalf("mismatch\nsrc:  %q\ndst:  %q\nslow: %q (err %v)\nfast: %q",
				src, dst, slowOut, slowErr, fastOut)
		}
	})
}
