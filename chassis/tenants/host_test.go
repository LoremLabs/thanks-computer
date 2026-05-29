package tenants

import "testing"

// TestCanonicalizeHost is table-driven over every edge case called out
// in the design — IPv6 with/without brackets, mixed case, trailing
// dot, ports, malformed multi-colon strings, whitespace, empties.
// Adding a row here is the right place to lock in any future host-
// parsing decision; the production code must never carry the cases
// inline.
func TestCanonicalizeHost(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		out    string
		ok     bool
	}{
		{"plain hostname", "example.com", "example.com", true},
		{"uppercase", "Example.COM", "example.com", true},
		{"trailing dot", "example.com.", "example.com", true},
		{"hostname with port", "example.com:8080", "example.com", true},
		{"uppercase + port", "EXAMPLE.com:8080", "example.com", true},
		{"trailing dot + port", "example.com.:8080", "example.com", true},
		{"ipv6 bracketed", "[::1]", "::1", true},
		{"ipv6 with port", "[::1]:8080", "::1", true},
		{"ipv6 bare (rejected)", "::1", "", false},
		{"malformed multi-colon", "host:bad:port", "", false},
		{"whitespace", "  example.com  ", "example.com", true},
		{"empty", "", "", false},
		{"only dot", ".", "", false},
		{"only whitespace", "   ", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CanonicalizeHost(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok: got %v, want %v (in=%q got=%q)", ok, tc.ok, tc.in, got)
			}
			if ok && got != tc.out {
				t.Errorf("canonical: got %q, want %q (in=%q)", got, tc.out, tc.in)
			}
		})
	}
}

// TestIsValidHostname covers the strict admin-write predicate that
// runs AFTER canonicalisation. It rejects IPs (which should go through
// YAML, not DB) and various junk shapes.
func TestIsValidHostname(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"plain", "example.com", true},
		{"single label", "localhost", true},
		{"hyphen in label", "api-v2.example.com", true},
		{"deep subdomain", "a.b.c.d.example.com", true},
		{"empty", "", false},
		{"leading hyphen", "-example.com", false},
		{"trailing hyphen", "example-.com", false},
		{"empty label", "example..com", false},
		{"ipv4 literal", "1.2.3.4", false},
		{"ipv6 literal", "::1", false},
		{"uppercase (not canonical)", "Example.com", false},
		{"too long", longLabel(254), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidHostname(tc.in); got != tc.ok {
				t.Errorf("IsValidHostname(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

func longLabel(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
