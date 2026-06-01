package tenants

import "testing"

// TestIsDevLocalHostname pins the patterns auto-verified on claim when
// dev-auto-verify-local-hostnames is on. Adding a pattern is a UX
// loosening (skips proof-of-ownership for a hostname class) so the
// table is the contract; tighten with care.
func TestIsDevLocalHostname(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// positive cases
		{"localhost", true},
		{"app.localhost", true},
		{"chat.localhost", true},
		{"deep.nested.localhost", true},
		{"foo.local", true},
		{"my-service.local", true},
		{"smoke.local.thanks.computer", true},
		{"ai-chat.local.thanks.computer", true},
		{"a.b.local.thanks.computer", true},

		// negative cases — public TLDs and operator domains must NOT match
		{"example.com", false},
		{"thanks.computer", false},
		{"app.thanks.computer", false}, // no `.local.` infix
		{"app.example.local.com", false},
		{"localhost.evil.com", false}, // suffix-injection attempt
		{"local.evil.com", false},

		// edge cases
		{"", false},
		{"localhost.", false}, // canonicalization strips trailing dot upstream; this raw form must not match
	}

	for _, c := range cases {
		got := IsDevLocalHostname(c.host)
		if got != c.want {
			t.Errorf("IsDevLocalHostname(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
