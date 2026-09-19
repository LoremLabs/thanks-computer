package include_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/include"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

// A script with everything a hand-escaped literal gets wrong.
const script = "#!/bin/sh\nset -e\necho \"hi\" 'there' \\\n  | tr a-z A-Z\t# tab\r\nprintf '%s\\n' \"$HOME\" {{ not a marker }} op://nope &include(\"x\") é\n"

const op = "OPS/app/0100_RUN/run.txcl"

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for k, v := range m {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return fsys
}

// lit lexes one string literal back to its value.
func lit(t *testing.T, q string) string {
	t.Helper()
	l := lexer.New(q)
	tok := l.NextToken()
	test.Equals(t, token.TokenType(token.STRING), tok.Type)
	test.Equals(t, token.TokenType(token.EOF), l.NextToken().Type)
	test.Equals(t, 0, len(l.Errors()))
	return tok.Literal
}

func TestQuoteRoundTrips(t *testing.T) {
	test.Equals(t, script, lit(t, include.Quote(script)))
	test.Equals(t, 0, strings.Count(include.Quote(script), "\n")) // one line
	test.Equals(t, `""`, include.Quote(""))
}

func FuzzQuote(f *testing.F) {
	for _, s := range []string{"", script, `\`, `"`, `\\"`, "a\\nb", "\x01\x7fé\U0001F600", "{{@x}}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			t.Skip()
		}
		if got := lit(t, include.Quote(s)); got != s {
			t.Fatalf("round trip: %q -> %q", s, got)
		}
	})
}

func TestExpandEverywhereALiteralGoes(t *testing.T) {
	src := `WHEN .x == &include("v.txt") && .y =~ /a/
  EXEC "workspace://w/exec"
  WITH args = ["python3", "-c", &include("run.py")], stdin = &include("../_lib/in.txt")
SET .s = &include("v.txt")
EMIT .e = &concat("<", &include ("v.txt"), ">")
# &include("not-me.txt") in a comment
EMIT .t = "&include(\"not-me.txt\") in a string"
`
	fsys := files(map[string]string{
		op:                        src,
		"OPS/app/0100_RUN/v.txt":  "vee",
		"OPS/app/0100_RUN/run.py": script,
		"OPS/app/_lib/in.txt":     "line1\nline2\n",
	})
	out, deps, err := include.Expand(fsys, "OPS/app", op, src)
	test.Ok(t, err)
	test.Equals(t, []string{"OPS/app/0100_RUN/v.txt", "OPS/app/0100_RUN/run.py", "OPS/app/_lib/in.txt"}, deps)
	test.Equals(t, strings.Count(src, "\n"), strings.Count(out, "\n")) // line numbers kept
	test.Assert(t, !include.Has(out), "expanded text still has a directive: %s", out)
	test.Assert(t, strings.Contains(out, `# &include("not-me.txt")`), "comment touched")
	test.Assert(t, strings.Contains(out, `"&include(\"not-me.txt\") in a string"`), "string touched")
	test.Equals(t, []string{}, txcl.Validate(out))

	// The script comes back byte for byte out of the parsed op.
	l := lexer.New(out)
	var strs []string
	for tok := l.NextToken(); tok.Type != token.EOF; tok = l.NextToken() {
		if tok.Type == token.STRING {
			strs = append(strs, tok.Literal)
		}
	}
	test.Assert(t, contains(strs, script), "script not in %q", strs)
	test.Assert(t, contains(strs, "line1\nline2\n"), "stdin not in %q", strs)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestExpandNoDirectiveIsUntouched(t *testing.T) {
	src := "EMIT .a = \"b\"\n"
	out, deps, err := include.Expand(files(nil), "OPS/app", op, src)
	test.Ok(t, err)
	test.Equals(t, src, out)
	test.Equals(t, 0, len(deps))
}

func TestExpandStripsBOM(t *testing.T) {
	fsys := files(map[string]string{"OPS/app/0100_RUN/p.md": "\xef\xbb\xbfHello"})
	out, _, err := include.Expand(fsys, "OPS/app", op, `EMIT .p = &include("p.md")`)
	test.Ok(t, err)
	test.Equals(t, `EMIT .p = "Hello"`, out)
}

func TestExpandRefusals(t *testing.T) {
	fsys := files(map[string]string{
		"OPS/app/0100_RUN/ok.txt":  "ok",
		"OPS/app/0100_RUN/bad.bin": "\xff\xfe",
		"OPS/app/0100_RUN/nul.txt": "a\x00b",
		"OPS/app/0100_RUN/big.txt": strings.Repeat("x", include.MaxBytes+1),
		"OPS/other/secret.txt":     "no",
	})
	fsys["OPS/app/0100_RUN/link.txt"] = &fstest.MapFile{Data: []byte("ok.txt"), Mode: os.ModeSymlink}
	fsys["OPS/app/0100_RUN/dir/x"] = &fstest.MapFile{Data: []byte("x")}
	for _, tc := range []struct{ src, want string }{
		{`EMIT .a = &include("missing.txt")`, `run.txcl:1: &include("missing.txt"): no such file OPS/app/0100_RUN/missing.txt`},
		{`EMIT .a = &include("/etc/passwd")`, "absolute path"},
		{`EMIT .a = &include("../../other/secret.txt")`, "outside the stack directory OPS/app/"},
		{`EMIT .a = &include("../../../etc/passwd")`, "outside the stack directory"},
		{`EMIT .a = &include("")`, "empty path"},
		{`EMIT .a = &include("link.txt")`, "is a symlink"},
		{`EMIT .a = &include("dir")`, "not a regular file"},
		{`EMIT .a = &include("bad.bin")`, "not UTF-8"},
		{`EMIT .a = &include("nul.txt")`, "NUL"},
		{`EMIT .a = &include("big.txt")`, "the limit is 1048576"},
		{"\n\nEMIT .a = &include(b64\"ok.txt\")", `run.txcl:3: &include takes one quoted path`},
		{`EMIT .a = &include("ok.txt", "ok.txt")`, "one quoted path"},
		{`EMIT .a = &include(.x)`, "one quoted path"},
		{`EMIT .a = &include`, "one quoted path"},
		{`EMIT .a = &include("ok.txt") EMIT .b = "unterminated`, "unterminated string"},
	} {
		_, _, err := include.Expand(fsys, "OPS/app", op, tc.src)
		test.Assert(t, err != nil && strings.Contains(err.Error(), tc.want), "%s: got %v, want %q", tc.src, err, tc.want)
	}
}

// A real directory symlink below the stack dir is refused too (os.DirFS
// follows intermediate links, so the check walks every segment).
func TestExpandRefusesDirSymlinkOnDisk(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	test.Ok(t, os.MkdirAll(outside, 0o755))
	test.Ok(t, os.WriteFile(filepath.Join(outside, "s.txt"), []byte("s"), 0o644))
	stack := filepath.Join(root, "OPS", "app")
	test.Ok(t, os.MkdirAll(filepath.Join(stack, "0100_RUN"), 0o755))
	test.Ok(t, os.Symlink(outside, filepath.Join(stack, "lib")))
	_, _, err := include.Expand(os.DirFS(root), "OPS/app", op, `EMIT .a = &include("../lib/s.txt")`)
	test.Assert(t, err != nil && strings.Contains(err.Error(), "OPS/app/lib is a symlink"), "got %v", err)
}

func TestHas(t *testing.T) {
	test.Assert(t, include.Has(`EMIT .a = &include("x")`), "directive")
	test.Assert(t, include.Has(`EMIT .a = &include`), "bare directive")
	test.Assert(t, !include.Has(`EMIT .a = "&include(\"x\")"`), "inside a string")
	test.Assert(t, !include.Has("# &include(\"x\")\nEMIT .a = 1"), "inside a comment")
	test.Assert(t, !include.Has(`EMIT .a = &include_b64("x")`), "another name")
}
