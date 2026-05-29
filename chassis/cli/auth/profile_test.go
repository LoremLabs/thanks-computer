package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadActiveProfileMissingDefault — no $TXCO_HOME/active file at
// all → DefaultProfile, never an error. Critical back-compat: every
// developer running before this work had no active file and must
// keep getting "local" picked for them.
func TestReadActiveProfileMissingDefault(t *testing.T) {
	withHome(t)
	got, err := ReadActiveProfile()
	if err != nil {
		t.Fatalf("ReadActiveProfile: %v", err)
	}
	if got != DefaultProfile {
		t.Errorf("got %q, want %q (back-compat default)", got, DefaultProfile)
	}
}

// TestActiveRoundtrip — write a name, read it back, modes are right.
func TestActiveRoundtrip(t *testing.T) {
	withHome(t)
	if err := WriteActiveProfile("work"); err != nil {
		t.Fatalf("WriteActiveProfile: %v", err)
	}
	got, err := ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if got != "work" {
		t.Errorf("got %q, want work", got)
	}
	// File should be 0600 — the name itself isn't sensitive, but
	// it lives next to credentials so mirror the conservative perm.
	p, _ := activePath()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("active file mode = %o, want 0600", perm)
	}
}

// TestActiveSentinelNone — writing ActiveNone is the logout state.
// ReadActiveProfile returns it verbatim so callers can distinguish
// "logged out" from "use the default".
func TestActiveSentinelNone(t *testing.T) {
	withHome(t)
	if err := WriteActiveProfile(ActiveNone); err != nil {
		t.Fatal(err)
	}
	got, err := ReadActiveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if got != ActiveNone {
		t.Errorf("got %q, want %q", got, ActiveNone)
	}
}

// TestWriteActiveRejectsBadNames — only validKeyName chars (or the
// ActiveNone sentinel). A bogus name would land on disk and break
// every subsequent command; catch it at write time.
func TestWriteActiveRejectsBadNames(t *testing.T) {
	withHome(t)
	for _, bad := range []string{"", "foo bar", "foo/bar", "../etc", "foo;rm"} {
		if err := WriteActiveProfile(bad); err == nil {
			t.Errorf("WriteActiveProfile(%q) should have failed", bad)
		}
	}
}

// TestResolveProfilePrecedence — flag > env > active file > default.
// All four branches are exercised in one tabular pass.
func TestResolveProfilePrecedence(t *testing.T) {
	withHome(t)
	// Active file says "work".
	if err := WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		flag   string
		envSet string
		want   string
	}{
		{"flag wins over env+active", "flagval", "envval", "flagval"},
		{"env wins over active", "", "envval", "envval"},
		{"active file when no flag/env", "", "", "work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet == "" {
				t.Setenv("TXCO_PROFILE", "")
			} else {
				t.Setenv("TXCO_PROFILE", tc.envSet)
			}
			got, err := ResolveProfile(tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveProfileFallsThroughToDefault — empty everything,
// no active file → DefaultProfile.
func TestResolveProfileFallsThroughToDefault(t *testing.T) {
	withHome(t)
	t.Setenv("TXCO_PROFILE", "")
	got, err := ResolveProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultProfile {
		t.Errorf("got %q, want %q", got, DefaultProfile)
	}
}

// TestListProfilesEnumeratesAndTagsActive — drop two meta files in
// place, mark one active, list them. Active comes first, both are
// returned, the active flag is set correctly.
func TestListProfilesEnumeratesAndTagsActive(t *testing.T) {
	withHome(t)
	// Seed two profiles by hand: one "local" and one "work".
	for _, n := range []string{"local", "work"} {
		mp, err := MetaPath(n)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Dir(mp), 0o700)
		if err := SaveMeta(mp, Meta{
			ActorID:    "actor_" + n,
			KeyID:      "key_" + n,
			ChassisURL: "http://x:" + n,
			EnrolledAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	out, err := ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d profiles, want 2", len(out))
	}
	if out[0].Name != "work" || !out[0].Active {
		t.Errorf("active profile should sort first; got first=%+v", out[0])
	}
	if out[1].Name != "local" || out[1].Active {
		t.Errorf("non-active profile should be tagged inactive; got %+v", out[1])
	}
}

// TestListProfilesEmptyKeysDir — no $TXCO_HOME/keys dir → empty
// slice, no error. Lets fresh installs run `txco auth profiles`
// without ceremony.
func TestListProfilesEmptyKeysDir(t *testing.T) {
	withHome(t)
	out, err := ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty profile list, got %d", len(out))
	}
}
