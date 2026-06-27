package auth

import (
	"flag"
	"testing"
)

// TestTrailingPositional mirrors the command pattern (primary arg at Arg(0), then
// re-parse the rest) and checks the leftover positional that selects the target.
func TestTrailingPositional(t *testing.T) {
	mk := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		_ = fs.String("tenant", "", "")
		_ = fs.Parse(args)
		if fs.NArg() >= 1 { // command captured Arg(0) as its primary, then re-parses
			_ = fs.Parse(fs.Args()[1:])
		}
		return fs
	}
	cases := map[string]struct {
		args []string
		want string
	}{
		"primary only":               {[]string{"NAME"}, ""},
		"primary + target":           {[]string{"NAME", "staging"}, "staging"},
		"primary + flag (no target)": {[]string{"NAME", "--tenant", "t"}, ""},
		"primary + flag + target":    {[]string{"NAME", "--tenant", "t", "staging"}, "staging"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := trailingPositional(mk(c.args)); got != c.want {
				t.Errorf("trailingPositional(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

func TestApplyTargetSelector(t *testing.T) {
	// A URL value → folded into url (profile untouched).
	u, p := "", ""
	applyTargetSelector("https://x:8081", &u, &p)
	if u != "https://x:8081" || p != "" {
		t.Errorf("url: got url=%q profile=%q", u, p)
	}

	// host:port (no scheme) is also a URL.
	u, p = "", ""
	applyTargetSelector("localhost:9", &u, &p)
	if u != "localhost:9" || p != "" {
		t.Errorf("host:port: got url=%q profile=%q", u, p)
	}

	// A bare name → folded into profile.
	u, p = "", ""
	applyTargetSelector("cloud", &u, &p)
	if p != "cloud" || u != "" {
		t.Errorf("name: got url=%q profile=%q", u, p)
	}

	// Explicit --url / --profile are NOT overridden by --target.
	u, p = "http://set", "myprof"
	applyTargetSelector("cloud", &u, &p)
	if u != "http://set" || p != "myprof" {
		t.Errorf("explicit preserved: got url=%q profile=%q", u, p)
	}

	// Empty --target is a no-op.
	u, p = "", ""
	applyTargetSelector("", &u, &p)
	if u != "" || p != "" {
		t.Errorf("empty noop: got url=%q profile=%q", u, p)
	}
}

func TestApplyTargetSelectorName(t *testing.T) {
	// A name overrides the command's non-empty default key name.
	u, n := "", "local"
	applyTargetSelectorName("cloud", &u, &n)
	if n != "cloud" || u != "" {
		t.Errorf("name override: got url=%q name=%q", u, n)
	}

	// A URL → url; the default name is left alone.
	u, n = "", "local"
	applyTargetSelectorName("https://x", &u, &n)
	if u != "https://x" || n != "local" {
		t.Errorf("url: got url=%q name=%q", u, n)
	}
}
