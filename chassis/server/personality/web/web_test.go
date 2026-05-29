package web

import (
	"reflect"
	"testing"
)

// TestSplitMocksHeader locks in the X-Txco-Mocks parsing shape: comma
// separated, whitespace-trimmed, empties dropped. The gate flag itself
// (Conf.WebMockHeader) is enforced at the call site; this test just
// covers the splitter so the comma/whitespace edge cases are nailed
// down without spinning up a full HTTP server.
func TestSplitMocksHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "hello-world/**", []string{"hello-world/**"}},
		{"comma list", "a,b,c", []string{"a", "b", "c"}},
		{"spaces around commas", "a, b , c", []string{"a", "b", "c"}},
		{"trailing comma", "a,b,", []string{"a", "b"}},
		{"all whitespace becomes nil", "   ", nil},
		{"empty entries dropped", "a,,b", []string{"a", "b"}},
		{"exclusion pattern preserved", "**,!hello-world/100/critical",
			[]string{"**", "!hello-world/100/critical"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitMocksHeader(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitMocksHeader(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
