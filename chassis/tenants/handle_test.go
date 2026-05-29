package tenants

import (
	"regexp"
	"strings"
	"testing"
)

// labelRE is the single-label form of host.go's hostnameRE — what the
// leftmost label MintHandle produces must satisfy.
var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func TestMintHandleProducesValidLabel(t *testing.T) {
	cases := []struct {
		stack    string
		wantHint string // expected sanitized hint prefix ("" = rand only)
	}{
		{"test-stack", "test-stack"},
		{"website/canary", "website-canary"},                // slash → label-safe
		{"My_Stack", "my-stack"},                            // upper/underscore
		{"  Weird__Name!! ", "weird-name"},                  // trim/collapse/strip
		{"___", ""},                                         // all punctuation → rand only
		{"a", "a"},                                          // minimal
		{strings.Repeat("x", 100), strings.Repeat("x", 30)}, // truncated ≤30
		{"-leading-and-trailing-", "leading-and-trailing"},
	}
	for _, c := range cases {
		h := MintHandle(c.stack)
		if !labelRE.MatchString(h) {
			t.Errorf("MintHandle(%q)=%q is not a valid DNS label", c.stack, h)
		}
		if len(h) > 63 {
			t.Errorf("MintHandle(%q)=%q exceeds 63 chars (%d)", c.stack, h, len(h))
		}
		// Full host must validate under both a prod-style and the dev
		// .localhost suffix.
		for _, suf := range []string{".stacks.thanks.computer", ".localhost"} {
			host := h + suf
			canon, ok := CanonicalizeHost(host)
			if !ok || !IsValidHostname(canon) {
				t.Errorf("host %q (from stack %q) failed canon/validate (ok=%v)", host, c.stack, ok)
			}
		}
		if c.wantHint == "" {
			// rand-only: no hint segment, so it equals randLabel shape.
			if strings.Contains(h, "-") && !labelRE.MatchString(h) {
				t.Errorf("MintHandle(%q)=%q expected rand-only", c.stack, h)
			}
		} else if !strings.HasPrefix(h, c.wantHint+"-") {
			t.Errorf("MintHandle(%q)=%q: want hint prefix %q-", c.stack, h, c.wantHint)
		}
	}
}

func TestMintHandleUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		h := MintHandle("web")
		if !strings.HasPrefix(h, "web-") {
			t.Fatalf("hint not stable: %q", h)
		}
		if seen[h] {
			t.Fatalf("collision after %d mints: %q", i, h)
		}
		seen[h] = true
	}
}

func TestSanitizeHint(t *testing.T) {
	for in, want := range map[string]string{
		"test-stack":     "test-stack",
		"website/canary": "website-canary",
		"A//B__C":        "a-b-c",
		"   ":            "",
		"-x-":            "x",
		"foo.bar":        "foo-bar",
	} {
		if got := sanitizeHint(in); got != want {
			t.Errorf("sanitizeHint(%q)=%q, want %q", in, got, want)
		}
	}
}
