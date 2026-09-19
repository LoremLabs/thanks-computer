package lexer_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

// Every token's span is exactly its own text: whitespace and comments
// before it are excluded, strings keep their quotes, &fn keeps its `&`.
func TestSpan(t *testing.T) {
	input := `# leading comment
WHEN .x.y == "a \"b\"" && .n =~ /^[a-z]+$/
  EXEC "txco://kv/get" # trailing
  WITH args = &array("python3", "-c", b64"hi"), n = -12.5, @web.status = 200
EMIT ._z = &include ("p.py")`
	want := []string{
		"WHEN", ".x.y", "==", `"a \"b\""`, "&&", ".n", "=~", "/^[a-z]+$/",
		"EXEC", `"txco://kv/get"`,
		"WITH", "args", "=", "&array", "(", `"python3"`, ",", `"-c"`, ",", `b64"hi"`, ")", ",",
		"n", "=", "-12.5", ",", "@web.status", "=", "200",
		"EMIT", "._z", "=", "&include", "(", `"p.py"`, ")",
	}
	l := lexer.New(input)
	var got []string
	for {
		tok := l.NextToken()
		start, end := l.Span()
		if tok.Type == token.EOF {
			test.Equals(t, len(input), start)
			test.Equals(t, len(input), end)
			break
		}
		got = append(got, input[start:end])
	}
	test.Equals(t, want, got)
}
