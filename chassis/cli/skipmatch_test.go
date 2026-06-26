package cli

import "testing"

func TestSkipMatch(t *testing.T) {
	cases := []struct {
		stack    string
		patterns []string
		want     string
	}{
		{"publications/the-three-musketeers", []string{"publications"}, "publications"},
		{"publications/the-three-musketeers", nil, ""},
		{"www", []string{"publications"}, ""},
		{"send-drip", []string{"publications", "drip"}, "drip"}, // first match wins (order)
		{"router", []string{""}, ""},                            // empty pattern never matches
		{"publications/eugene-onegin", []string{"onegin"}, "onegin"},
	}
	for _, tc := range cases {
		if got := skipMatch(tc.stack, tc.patterns); got != tc.want {
			t.Errorf("skipMatch(%q, %v) = %q, want %q", tc.stack, tc.patterns, got, tc.want)
		}
	}
}
