package include_test

import (
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/include"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

// canon is a JSON text's canonical form through Go: sorted keys, compact.
func canon(t *testing.T, js string) string {
	t.Helper()
	var v interface{}
	test.Ok(t, json.Unmarshal([]byte(js), &v))
	b, err := json.Marshal(v)
	test.Ok(t, err)
	return string(b)
}

// litJSON is a parsed literal's value in the same canonical form, so an
// expanded value can be compared with the file it came from.
func litJSON(t *testing.T, v ast.Value) string {
	t.Helper()
	l, ok := v.(ast.Literal)
	test.Assert(t, ok, "not a literal: %#v", v)
	b, err := json.Marshal(l.V)
	test.Ok(t, err)
	return string(b)
}

func expandOne(t *testing.T, fsys fs.FS, src string) string {
	t.Helper()
	out, _, err := include.Expand(fsys, "OPS/app", op, src)
	test.Ok(t, err)
	return out
}

// A `$[...]` works in every position a literal does, keeps the value's
// JSON type, is one line, and is text inside a string or a comment.
func TestExpandFileLiteralEverywhere(t *testing.T) {
	src := `WHEN .x in $[nouns.json] && .y == "$[not.json]" && .n == $[one.json]
SET .s = $[str.json], .nothing = $[null.json], .t = $[bool.json]
  EXEC "ai://chat"
  WITH schema = $[schemas/reply.json], args = ["python3", $[a.json]], up = $[../_shared/up.json]
# $[comment.json] in a comment
EMIT .o = $[nested.json]
`
	const schema = `{"type": "object", "properties": {"reply": {"type": "string"}}, "required": ["reply"]}`
	const nested = `{"a": {"b": [1, {"c": null}]}, "d": [[]], "e": {}, "f": -2.5}`
	fsys := files(map[string]string{
		op:                                    src,
		"OPS/app/0100_RUN/nouns.json":         "[\"seed\", \"run\"]\n",
		"OPS/app/0100_RUN/one.json":           "1",
		"OPS/app/0100_RUN/str.json":           `"hello \"there\"\n"`,
		"OPS/app/0100_RUN/null.json":          "null",
		"OPS/app/0100_RUN/bool.json":          "false",
		"OPS/app/0100_RUN/schemas/reply.json": schema,
		"OPS/app/0100_RUN/a.json":             "[1, 2.5, \"x\"]",
		"OPS/app/_shared/up.json":             `{"up": true}`,
		"OPS/app/0100_RUN/nested.json":        nested,
	})
	out, deps, err := include.Expand(fsys, "OPS/app", op, src)
	test.Ok(t, err)
	test.Equals(t, []string{
		"OPS/app/0100_RUN/nouns.json", "OPS/app/0100_RUN/one.json",
		"OPS/app/0100_RUN/str.json", "OPS/app/0100_RUN/null.json", "OPS/app/0100_RUN/bool.json",
		"OPS/app/0100_RUN/schemas/reply.json", "OPS/app/0100_RUN/a.json", "OPS/app/_shared/up.json",
		"OPS/app/0100_RUN/nested.json",
	}, deps)
	test.Equals(t, strings.Count(src, "\n"), strings.Count(out, "\n")) // line numbers kept
	test.Assert(t, !include.Has(out), "expanded text still has a directive: %s", out)
	test.Assert(t, strings.Contains(out, `"$[not.json]"`), "string touched: %s", out)
	test.Assert(t, strings.Contains(out, `# $[comment.json] in a comment`), "comment touched: %s", out)
	test.Equals(t, []string{}, txcl.Validate(out))

	res, err := txcl.Resonator(out)
	test.Ok(t, err)
	and := res.When.Expr.And
	test.Equals(t, 3, len(and))
	test.Equals(t, []interface{}{"seed", "run"}, and[0].Leaf.MatchValue)
	test.Equals(t, int64(1), and[2].Leaf.MatchValue)
	set := res.SetPre.Overrides
	test.Equals(t, "hello \"there\"\n", set[0].Value.(ast.Literal).V)
	test.Equals(t, nil, set[1].Value.(ast.Literal).V)
	test.Equals(t, false, set[2].Value.(ast.Literal).V)
	test.Equals(t, canon(t, schema), litJSON(t, res.With["schema"]))
	test.Equals(t, `["python3",[1,2.5,"x"]]`, litJSON(t, res.With["args"]))
	test.Equals(t, `{"up":true}`, litJSON(t, res.With["up"]))
	test.Equals(t, canon(t, nested), litJSON(t, res.Emit.Overrides[0].Value))
}

// The same bytes always expand to the same text: dev's fingerprint, the
// install hash and ops_uniq all hash it. Whitespace, line endings and a
// BOM never reach the output; the file's key order does.
func TestFileLiteralDeterministic(t *testing.T) {
	const v = "{\n  \"b\": 1,\n  \"a\": {\"c\": [true, null, \"x\"]},\n  \"n\": -2.50\n}\n"
	const want = `SET .x = {"b":1,"a":{"c":[true,null,"x"]},"n":-2.5}`
	one := func(file string) string {
		return expandOne(t, files(map[string]string{"OPS/app/0100_RUN/v.json": file}), `SET .x = $[v.json]`)
	}
	test.Equals(t, want, one(v))
	test.Equals(t, want, one(v))

	dir := t.TempDir()
	test.Ok(t, os.MkdirAll(filepath.Join(dir, "OPS/app/0100_RUN"), 0o755))
	test.Ok(t, os.WriteFile(filepath.Join(dir, "OPS/app/0100_RUN/v.json"), []byte(v), 0o644))
	test.Equals(t, want, expandOne(t, os.DirFS(dir), `SET .x = $[v.json]`))

	for _, variant := range []string{
		`{"b":1,"a":{"c":[true,null,"x"]},"n":-2.5}`,
		"{\r\n\t\"b\" : 1 ,\r\n\t\"a\" : { \"c\" : [ true , null , \"x\" ] } ,\r\n\t\"n\" : -2.5\r\n}\r\n",
		"\xef\xbb\xbf" + `{"b":1,"a":{"c":[true,null,"x"]},"n":-2.5}`,
	} {
		test.Equals(t, want, one(variant))
	}
	test.Equals(t, `SET .x = {"a":{"c":[true,null,"x"]},"b":1,"n":-2.5}`,
		one(`{"a":{"c":[true,null,"x"]},"b":1,"n":-2.5}`))

	test.Equals(t, `SET .x = [1,2.5,"x",[],{}]`, one("[1, 2.5, \"x\", [], {}]"))
	test.Equals(t, `SET .x = "s"`, one(`"s"`))
	test.Equals(t, `SET .x = null`, one("null"))
	test.Equals(t, `SET .x = true`, one(" true "))
}

// The lossless numeric domain: integers within ±2^53 as digits, other
// numbers as the shortest plain decimal that reads back to the same
// float64, everything else refused with the rule in the message.
func TestFileLiteralNumbers(t *testing.T) {
	expand := func(n string) (string, error) {
		fsys := files(map[string]string{"OPS/app/0100_RUN/n.json": n})
		out, _, err := include.Expand(fsys, "OPS/app", op, `SET .x = $[n.json]`)
		return out, err
	}
	accepted := []struct{ in, want string }{
		{"0", "0"}, {"-0", "0"}, {"1", "1"}, {"-17", "-17"},
		{"1.0", "1.0"}, {"0.1", "0.1"}, {"-2.50", "-2.5"}, {"123456.789", "123456.789"},
		{"1e5", "100000.0"}, {"1E-7", "0.0000001"}, {"1.5e3", "1500.0"}, {"2.5E+2", "250.0"},
		{"9007199254740992", "9007199254740992"}, {"-9007199254740992", "-9007199254740992"},
		{"0.000000000000001", "0.000000000000001"}, {"0.0", "0.0"}, {"-0.0", "-0.0"},
		{"1700000000000", "1700000000000"}, {"1700000000.123", "1700000000.123"},
	}
	for _, tc := range accepted {
		out, err := expand(tc.in)
		test.Ok(t, err)
		test.Equals(t, "SET .x = "+tc.want, out)
		l := lexer.New(tc.want)
		tok := l.NextToken()
		test.Assert(t, tok.Type == token.INT || tok.Type == token.FLOAT, "%s lexes as %s", tc.want, tok.Type)
		test.Equals(t, token.TokenType(token.EOF), l.NextToken().Type)
		want, _ := strconv.ParseFloat(tc.in, 64)
		got, _ := strconv.ParseFloat(tok.Literal, 64)
		test.Assert(t, want == got, "%s -> %s reads back as %v, want %v", tc.in, tc.want, got, want)
		if strings.Contains(tc.want, ".") && want != 0 {
			test.Equals(t, math.Float64bits(want), math.Float64bits(got))
		}
	}
	rejected := []struct{ in, want string }{
		{"9007199254740993", "outside ±2^53"},
		{"-9007199254740993", "outside ±2^53"},
		{"9223372036854775807", "outside ±2^53"},
		{"9223372036854775808", "outside ±2^53"},
		{"1e16", "outside the lossless range"},
		{"9007199254740992.0", "outside the lossless range"},
		{"1e300", "outside the lossless range"},
		{"5e-324", "outside the lossless range"},
		{"1e-16", "outside the lossless range"},
		{"1e400", "outside float64's range"},
	}
	for _, tc := range rejected {
		_, err := expand(tc.in)
		test.Assert(t, err != nil && strings.Contains(err.Error(), tc.want), "%s: got %v, want %q", tc.in, err, tc.want)
	}
}

// JSON's escapes become UTF-8, then txcl's escapes; raw non-ASCII passes
// through; a directive spelled inside a JSON string is text.
func TestFileLiteralUnicodeAndEscapes(t *testing.T) {
	cases := []struct{ name, file, want string }{
		{"u-escape", `"café"`, "café"},
		{"surrogate pair", `"😀"`, "😀"},
		{"raw non-ascii", `"日本語 😀 é"`, "日本語 😀 é"},
		{"solidus", `"a\/b"`, "a/b"},
		{"backspace formfeed", `"\b\f"`, "\b\f"},
		{"control", `"\u0001\u001f"`, "\x01\x1f"},
		{"txcl escapes", `"q\"b\\n\nr\rt\t"`, "q\"b\\n\nr\rt\t"},
		{"directives are text", `"$[other.json] &include(\"x\") # not a comment"`, `$[other.json] &include("x") # not a comment`},
		{"quotes", `"\"\"\""`, `"""`},
		{"empty", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := files(map[string]string{"OPS/app/0100_RUN/u.json": tc.file})
			out := expandOne(t, fsys, "EMIT .s = $[u.json]\nEMIT .after = 1")
			test.Equals(t, 1, strings.Count(out, "\n"))
			test.Equals(t, []string{}, txcl.Validate(out))
			res, err := txcl.Resonator(out)
			test.Ok(t, err)
			test.Equals(t, tc.want, res.Emit.Overrides[0].Value.(ast.Literal).V)
			test.Equals(t, int64(1), res.Emit.Overrides[1].Value.(ast.Literal).V)
		})
	}
	// Keys go through the same escaping.
	fsys := files(map[string]string{"OPS/app/0100_RUN/k.json": `{"k \"q\" \\ é\n": 1, "日本": 2}`})
	out := expandOne(t, fsys, `EMIT .o = $[k.json]`)
	test.Equals(t, []string{}, txcl.Validate(out))
	res, err := txcl.Resonator(out)
	test.Ok(t, err)
	test.Equals(t, `{"k \"q\" \\ é\n":1,"日本":2}`, litJSON(t, res.Emit.Overrides[0].Value))
}

func TestFileLiteralRefusals(t *testing.T) {
	fsys := files(map[string]string{
		"OPS/app/0100_RUN/ok.json":    `[1]`,
		"OPS/app/0100_RUN/notes.txt":  `[1]`,
		"OPS/app/0100_RUN/UP.JSON":    `[1]`,
		"OPS/app/0100_RUN/bad.json":   "[1,\n 2,,]",
		"OPS/app/0100_RUN/open.json":  `{"a": [1, 2`,
		"OPS/app/0100_RUN/empty.json": "",
		"OPS/app/0100_RUN/blank.json": " \n\t\n",
		"OPS/app/0100_RUN/trail.json": `[1] [2]`,
		"OPS/app/0100_RUN/dup.json":   `{"a": 1, "b": 2, "a": 3}`,
		"OPS/app/0100_RUN/nul.json":   `"a\u0000b"`,
		"OPS/app/0100_RUN/lone.json":  `"\ud800x"`,
		"OPS/app/0100_RUN/deep.json":  strings.Repeat("[", 129) + strings.Repeat("]", 129),
		"OPS/app/0100_RUN/fine.json":  strings.Repeat("[", 128) + strings.Repeat("]", 128),
	})
	for _, tc := range []struct{ src, want string }{
		{`SET .x = $[missing.json]`, `run.txcl:1: $[missing.json]: no such file OPS/app/0100_RUN/missing.json`},
		{`SET .x = $[notes.txt]`, `run.txcl:1: $[notes.txt]: the file must be named *.json, in lower case`},
		{`SET .x = $[UP.JSON]`, `must be named *.json, in lower case`},
		{`SET .x = $[my file.json]`, `unsupported character ' '`},
		{`SET .x = $[]`, `$[]: empty path`},
		{`SET .x = $[/etc/x.json]`, `absolute path`},
		{`SET .x = $[../../other/x.json]`, `outside the stack directory OPS/app/`},
		{"SET .x = $[ok.json\nEMIT .y = 1", `run.txcl:1: $[ok.json]: unterminated $[...] file literal`},
		{"SET .x = 1\nSET .y = $[bad.json]", `run.txcl:2: $[bad.json]: invalid JSON at line 2, column`},
		{`SET .x = $[bad.json]`, `invalid character ',' looking for beginning of value`},
		{`SET .x = $[open.json]`, `invalid JSON at line 1, column`},
		{`SET .x = $[open.json]`, `unexpected end of file`},
		{`SET .x = $[empty.json]`, `$[empty.json]: empty file; expected one JSON value`},
		{`SET .x = $[blank.json]`, `empty file`},
		{`SET .x = $[trail.json]`, `trailing data after the JSON value`},
		{`SET .x = $[dup.json]`, `duplicate key "a"`},
		{`SET .x = $[nul.json]`, `\u0000`},
		{`SET .x = $[lone.json]`, `lone surrogate`},
		{`SET .x = $[deep.json]`, `nested deeper than 128 levels`},
	} {
		_, _, err := include.Expand(fsys, "OPS/app", op, tc.src)
		test.Assert(t, err != nil && strings.Contains(err.Error(), tc.want), "%s: got %v, want %q", tc.src, err, tc.want)
	}
	// Exactly at the depth cap is fine.
	expandOne(t, fsys, `SET .x = $[fine.json]`)
}

// The admin API's guard sees both directives, by token.
func TestUnexpandedNamesTheDirective(t *testing.T) {
	d, ok := include.Unexpanded(`WHEN .x in $[a.json]`)
	test.Equals(t, true, ok)
	test.Equals(t, "$[...]", d)
	d, ok = include.Unexpanded(`SET .x = &include("a")`)
	test.Equals(t, true, ok)
	test.Equals(t, "&include", d)
	_, ok = include.Unexpanded(`SET .x = "$[a.json] &include(\"a\")" # $[c.json] &include("c")`)
	test.Equals(t, false, ok)
	_, ok = include.Unexpanded(`SET .x = 1`)
	test.Equals(t, false, ok)
	test.Assert(t, include.Has(`WHEN .x in $[a.json]`), "Has misses $[...]")
}

// Random JSON values either expand to txcl that parses clean and resolves
// to the same value, or are refused by a domain rule.
func FuzzJSONToTxcl(f *testing.F) {
	for _, s := range []string{
		`[]`, `{}`, `null`, `"s"`, `1`, `-2.5`, `1e5`, `[1, {"a": [true, null]}]`,
		`{"k \"q\"": "é\n"}`, `9007199254740993`, `"\ud800"`, `[[[[]]]]`, `{"a":1,"a":2}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !json.Valid([]byte(s)) || strings.IndexByte(s, 0) >= 0 {
			t.Skip()
		}
		fsys := files(map[string]string{"OPS/app/0100_RUN/v.json": s})
		out, _, err := include.Expand(fsys, "OPS/app", op, `SET .x = $[v.json]`)
		if err != nil {
			return // a refusal is fine; it must be one of ours, not a panic
		}
		if diags := txcl.Validate(out); len(diags) != 0 {
			t.Fatalf("%q -> %q does not validate: %v", s, out, diags)
		}
		res, err := txcl.Resonator(out)
		if err != nil {
			t.Fatalf("%q -> %q does not parse: %v", s, out, err)
		}
		var want interface{}
		if err := json.Unmarshal([]byte(s), &want); err != nil {
			t.Skip()
		}
		got := res.SetPre.Overrides[0].Value.(ast.Literal).V
		// Compare as JSON values, so int64 1 equals float64 1 and the
		// integer -0 (written as 0: int64 has no negative zero) equals 0.
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		var gv, wv interface{}
		_ = json.Unmarshal(gb, &gv)
		_ = json.Unmarshal(wb, &wv)
		if !reflect.DeepEqual(gv, wv) {
			t.Fatalf("%q -> %q resolves to %s, want %s", s, out, gb, wb)
		}
	})
}
