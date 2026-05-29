package policy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
)

// TestCanonical normalises every input shape to the storage form.
// admin:all and bare "*" expand to *:*:*; 2-segment legacy strings
// get an instance wildcard injected; already-3-segment strings pass
// through; malformed strings are left as-is (validator catches them).
func TestCanonical(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"admin:all", "*:*:*"},
		{"*", "*:*:*"},
		{"opstack:read", "opstack:*:read"},
		{"opstack:*:read", "opstack:*:read"},
		{"actor:abc:invite", "actor:abc:invite"},
		{"", ""},
		{"a:b:c:d", "a:b:c:d"}, // malformed → unchanged
		{"  opstack:read  ", "opstack:*:read"},
	}
	for _, tc := range cases {
		if got := Canonical(tc.in); got != tc.want {
			t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseCapabilitiesHappy — comma split, trim, dedupe, canonical
// form, sorted output. The sort is non-obvious: deterministic JSON
// storage requires a stable order.
func TestParseCapabilitiesHappy(t *testing.T) {
	got, err := ParseCapabilities("opstack:*:read,  actor:*:read , opstack:*:read")
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}
	want := []string{"actor:*:read", "opstack:*:read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseCapabilitiesNormalisesLegacy — the CLI still accepts the
// 2-segment habit and canonicalises silently so users don't have to
// retype every flag they're used to.
func TestParseCapabilitiesNormalisesLegacy(t *testing.T) {
	got, err := ParseCapabilities("opstack:read, admin:all")
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}
	want := []string{"*:*:*", "opstack:*:read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestParseCapabilitiesRejectsTypo — a string outside KnownCapabilities
// errors at parse time so the typo never reaches storage.
func TestParseCapabilitiesRejectsTypo(t *testing.T) {
	_, err := ParseCapabilities("opstack:reed")
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("got %v, want ErrUnknownCapability", err)
	}
}

// TestParseCapabilitiesEmpty — empty input is valid; the caller
// applies its own default (admin:all on the invite handler).
func TestParseCapabilitiesEmpty(t *testing.T) {
	got, err := ParseCapabilities("")
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for empty input", got)
	}
}

// TestMatcher3Segment — segment-by-segment wildcard matching. Each
// `*` in the grant matches anything in the want; otherwise exact.
// The aliases admin:all and bare "*" both behave as *:*:*.
func TestMatcher3Segment(t *testing.T) {
	cases := []struct {
		grant, want string
		ok          bool
	}{
		// Exact / wildcard combinations.
		{"opstack:*:read", "opstack:*:read", true},
		{"opstack:*:*", "opstack:*:read", true},
		{"opstack:*:*", "opstack:*:update", true},
		{"opstack:*:read", "opstack:*:update", false},
		{"opstack:*:read", "actor:*:read", false},

		// Aliases.
		{"admin:all", "opstack:*:update", true},
		{"*", "actor:*:revoke", true},
		{"*:*:*", "anything:goes:here", true},

		// Per-instance: matches only when instance segment equals or is *.
		{"opstack:abc:read", "opstack:abc:read", true},
		{"opstack:abc:read", "opstack:xyz:read", false},
		{"opstack:*:read", "opstack:abc:read", true},

		// 2-segment legacy on either side normalises with `*` instance.
		{"opstack:read", "opstack:*:read", true},
		{"opstack:*:read", "opstack:read", true},
	}
	for _, tc := range cases {
		wantSegs := segments(tc.want)
		got := matches(tc.grant, tc.want, wantSegs)
		if got != tc.ok {
			t.Errorf("grant=%q want=%q: got ok=%v, want %v", tc.grant, tc.want, got, tc.ok)
		}
	}
}

// TestLegacyAdminAllStillMatches3Segment — an actor with the legacy
// admin:all in their stored Capabilities still passes a 3-segment
// RequireCapability check. No data migration of pre-phase-6 rows
// required.
func TestLegacyAdminAllStillMatches3Segment(t *testing.T) {
	ctx := auth.WithContext(context.Background(), &auth.Context{
		Source:       "signed",
		Capabilities: []string{"admin:all"},
	})
	for _, want := range []string{"opstack:*:read", "opstack:*:update", "actor:*:invite"} {
		if err := RequireCapability(ctx, want); err != nil {
			t.Errorf("admin:all should match %q; got %v", want, err)
		}
	}
}

// TestCoversAllSubsetRules — `admin:all` covers everything; a more
// specific grant covers only itself + subsets via wildcard expansion.
// Returns the first uncovered want as the deny signal.
func TestCoversAllSubsetRules(t *testing.T) {
	cases := []struct {
		name        string
		grants      []string
		wants       []string
		wantMissing string // "" means all covered
	}{
		{
			name:        "admin:all covers everything",
			grants:      []string{"admin:all"},
			wants:       []string{"opstack:*:read", "opstack:*:update", "actor:*:invite"},
			wantMissing: "",
		},
		{
			name:        "opstack:*:* covers opstack reads + updates but not actor verbs",
			grants:      []string{"opstack:*:*"},
			wants:       []string{"opstack:*:read", "opstack:*:update"},
			wantMissing: "",
		},
		{
			name:        "opstack:*:* does NOT cover actor:*:invite",
			grants:      []string{"opstack:*:*"},
			wants:       []string{"opstack:*:read", "actor:*:invite"},
			wantMissing: "actor:*:invite",
		},
		{
			name:        "actor:*:invite alone cannot grant admin:all",
			grants:      []string{"actor:*:invite"},
			wants:       []string{"admin:all"},
			wantMissing: "admin:all",
		},
		{
			name:        "combined grants cover the union",
			grants:      []string{"opstack:*:read", "actor:*:invite"},
			wants:       []string{"opstack:*:read", "actor:*:invite"},
			wantMissing: "",
		},
		{
			name:        "exact-match grant covers itself",
			grants:      []string{"opstack:*:read"},
			wants:       []string{"opstack:*:read"},
			wantMissing: "",
		},
		{
			name:        "empty grants fails for any want",
			grants:      nil,
			wants:       []string{"opstack:*:read"},
			wantMissing: "opstack:*:read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CoversAll(tc.grants, tc.wants); got != tc.wantMissing {
				t.Errorf("CoversAll(%v, %v) = %q, want %q",
					tc.grants, tc.wants, got, tc.wantMissing)
			}
		})
	}
}

// TestValidateCapabilitiesWhitelist — every documented capability
// string is in the whitelist; an unknown 3-segment string fails.
func TestValidateCapabilitiesWhitelist(t *testing.T) {
	good := []string{
		"admin:all", "*", "*:*:*",
		"opstack:*:read", "opstack:*:update", "opstack:*:*",
		"actor:*:read", "actor:*:invite", "actor:*:revoke", "actor:*:*",
	}
	if err := ValidateCapabilities(good); err != nil {
		t.Fatalf("documented whitelist failed: %v", err)
	}
	bad := []string{"opstack:*:read", "made:*:up"}
	if err := ValidateCapabilities(bad); !errors.Is(err, ErrUnknownCapability) {
		t.Errorf("expected ErrUnknownCapability for unknown; got %v", err)
	}
}
