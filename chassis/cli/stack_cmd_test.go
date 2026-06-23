package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunStackSetFlagValidation covers the argument/flag checks that return
// before any network or workspace resolution, so they need no chassis.
func TestRunStackSetFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"both flags", []string{"--no-host", "--host", "s"}, 2},
		{"neither flag", []string{"s"}, 2},
		{"missing stack arg", []string{"--no-host"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if got := runStackSet(tc.args, &out, &errb); got != tc.want {
				t.Fatalf("runStackSet(%v) = %d, want %d (stderr=%s)", tc.args, got, tc.want, errb.String())
			}
		})
	}
}

// TestFormatLiveHosts: the 409 detail's hostnames render as " (a, b)", and
// missing/empty/malformed detail renders as "".
func TestFormatLiveHosts(t *testing.T) {
	cases := []struct {
		name   string
		detail map[string]any
		want   string
	}{
		{"two hosts", map[string]any{"hostnames": []any{"a.localhost", "b.localhost"}}, " (a.localhost, b.localhost)"},
		{"one host", map[string]any{"hostnames": []any{"x.dripl.it"}}, " (x.dripl.it)"},
		{"nil detail", nil, ""},
		{"missing key", map[string]any{"hint": "..."}, ""},
		{"empty list", map[string]any{"hostnames": []any{}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatLiveHosts(tc.detail); got != tc.want {
				t.Fatalf("formatLiveHosts(%v) = %q, want %q", tc.detail, got, tc.want)
			}
		})
	}
}

// TestRunStackUnknownSubcommand: `txco stack <bogus>` and bare `txco stack`
// are usage errors (exit 2).
func TestRunStackUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	if got := runStack([]string{"frobnicate"}, &out, &errb); got != 2 {
		t.Fatalf("unknown subcommand: got %d, want 2", got)
	}
	if !strings.Contains(errb.String(), "unknown subcommand") {
		t.Fatalf("stderr missing diagnostic: %s", errb.String())
	}
	errb.Reset()
	if got := runStack(nil, &out, &errb); got != 2 {
		t.Fatalf("no subcommand: got %d, want 2", got)
	}
}
