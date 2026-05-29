package lexer_test

// Regenerates the JSON fixtures the admin-ui TypeScript lexer port
// uses for parity testing. Run:
//
//   go test ./chassis/txcl/lexer/... -run TestGenerateFixtures -update
//
// Without -update the test is a no-op so it stays out of normal CI
// runs. Fixtures land in admin-ui/src/lib/txcl/__fixtures__/ relative
// to the repo root — the test resolves the path from its own location
// so it works regardless of working directory.

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

var updateFixtures = flag.Bool("update", false, "regenerate TS-lexer parity fixtures")

type fixtureToken struct {
	Type    string `json:"type"`
	Literal string `json:"literal"`
}

type fixture struct {
	Name   string         `json:"name"`
	Input  string         `json:"input"`
	Tokens []fixtureToken `json:"tokens"`
}

// lexAll drives the Go lexer to completion and returns the token stream
// up to (and including) EOF. Mirrors what the TS port's `lexAll` must do.
func lexAll(input string) []fixtureToken {
	l := lexer.New(input)
	out := []fixtureToken{}
	for {
		tok := l.NextToken()
		out = append(out, fixtureToken{Type: string(tok.Type), Literal: tok.Literal})
		if tok.Type == token.EOF {
			break
		}
	}
	return out
}

// Fixture inputs — keep ASCII-only so the TS port can operate on JS
// strings (UTF-16) without byte-encoding gymnastics. Cover every
// production from chassis/txcl/lexer/lexer.go: keywords (upper +
// lower), all operators, `.` and `@` BRANCH forms with quoted
// segments, `b64"..."`, regex literals after `=~`/`!~`, `*` after
// `WHEN` vs elsewhere, negative-leading numbers, string escapes,
// comments at line start / trailing / blank-line-only.
var fixtureInputs = []struct {
	name  string
	input string
}{
	{
		name: "screenshot-example",
		input: `# mcp-test
SELECT @web.req.url.query.question.0
    AS .question
    DEFAULT "What is react used for?",
    @web.req.url.query.repoName.0
    AS .repoName
    DEFAULT "facebook/react"

EXEC "mcp+https://mcp.deepwiki.com/mcp#ask_question" WITH timeout = "60s", debug = true
`,
	},
	{
		name:  "empty",
		input: "",
	},
	{
		name:  "comments-only",
		input: "# one\n# two\n  # indented\n",
	},
	{
		name:  "keywords-upper",
		input: "SELECT AS DEFAULT WITH WHEN PRIORITY EXEC EMIT SET RETURN NULL",
	},
	{
		name:  "keywords-lower",
		input: "select as default with when priority exec emit set return null fn let if else true false",
	},
	{
		name:  "operators",
		input: ".a == .b != .c < .d <= .e > .f >= .g && .h || !.i = .j + .k - .l * .m / .n",
	},
	{
		name:  "match-and-regex",
		input: `.x =~ /foo\/bar/ && .y !~ /^z+$/`,
	},
	{
		name:  "when-star",
		input: "WHEN * SET .ok = true",
	},
	{
		name:  "branches",
		input: `.foo .foo.bar .foo-bar.baz @web.req.url.query.q.0 @web.res.headers."content-type".0`,
	},
	{
		name:  "b64-string",
		input: `SET .blob = b64"hello world"`,
	},
	{
		name:  "numbers",
		input: "1 12 -3 -45 1.5 -2.75",
	},
	{
		name:  "string-escapes",
		input: `"plain" "with \"quote\"" "newline:\n" "tab:\t" "backslash:\\" "unknown:\z"`,
	},
	{
		name: "exec-with-options",
		input: `EXEC "http://example.com/api" WITH timeout = "60s", debug = true, retries = 3
`,
	},
	{
		name:  "trailing-comment",
		input: "SELECT .x AS .y # tail comment\nDEFAULT \"v\"\n",
	},
	{
		name: "brackets-and-braces",
		input: `SET .arr = [1, 2, 3], .obj = {"k": "v"}`,
	},
}

func TestGenerateFixtures(t *testing.T) {
	if !*updateFixtures {
		t.Skip("pass -update to regenerate TS lexer parity fixtures")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// chassis/txcl/lexer/fixtures_test.go → repo root is four parents up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	outDir := filepath.Join(repoRoot, "admin-ui", "src", "lib", "txcl", "__fixtures__")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", outDir, err)
	}

	written := []string{}
	for _, fx := range fixtureInputs {
		f := fixture{Name: fx.name, Input: fx.input, Tokens: lexAll(fx.input)}
		data, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", fx.name, err)
		}
		// Trailing newline so the file is POSIX-clean and git diffs are
		// tidy when a single fixture changes.
		data = append(data, '\n')
		path := filepath.Join(outDir, fx.name+".json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		written = append(written, fx.name+".json")
	}

	// Index lets the TS test enumerate fixtures without a directory
	// listing — keeps the test runner browser-portable in case we ever
	// move it off Node.
	indexData, err := json.MarshalIndent(written, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	indexData = append(indexData, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "index.json"), indexData, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	t.Logf("wrote %d fixtures to %s", len(written), outDir)
}
