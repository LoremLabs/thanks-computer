package tcp

import "testing"

// TestParseTCPListenSpec pins the back-compat contract: a bare
// address keeps stamping `_txc.tcp.listener = "default"` so existing
// configs (and ingress YAML keyed on "default") keep working; a
// `name=addr` entry opts into per-listener routing.
func TestParseTCPListenSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantAddr string
	}{
		{":5050", "default", ":5050"},
		{"127.0.0.1:5050", "default", "127.0.0.1:5050"},
		{"[::1]:5050", "default", "[::1]:5050"},
		{"webhooks=:5050", "webhooks", ":5050"},
		{"iot=127.0.0.1:5051", "iot", "127.0.0.1:5051"},
		{"  spaced  =  :5050  ", "spaced", ":5050"},
		{"=:5050", "default", ":5050"}, // empty name → fall back to "default"
		{"", "", ""},                    // blank entries are skipped by the caller
		{"   ", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotName, gotAddr := parseTCPListenSpec(tc.in)
			if gotName != tc.wantName || gotAddr != tc.wantAddr {
				t.Errorf("parseTCPListenSpec(%q) = (%q,%q), want (%q,%q)",
					tc.in, gotName, gotAddr, tc.wantName, tc.wantAddr)
			}
		})
	}
}
