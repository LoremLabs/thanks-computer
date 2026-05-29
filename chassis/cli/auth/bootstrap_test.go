package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSuggestProfileNameFromCwdHappy — running from a directory whose
// basename is a valid profile name returns the basename. Mirrors the
// "I'm in examples/quickstart-hello-world and I want a fresh profile"
// case the collision-recovery prompt is built for.
func TestSuggestProfileNameFromCwdHappy(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "quickstart-hello-world")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withCwd(t, dir, func() {
		got := suggestProfileNameFromCwd("local")
		if got != "quickstart-hello-world" {
			t.Errorf("got %q, want quickstart-hello-world", got)
		}
	})
}

// TestSuggestProfileNameFromCwdInvalidBasename — basenames with
// shell-unsafe characters (spaces, dots) get rejected. The function
// returns "" so the caller's prompt renders without a suggested
// default rather than offering a name the user can't actually use.
func TestSuggestProfileNameFromCwdInvalidBasename(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "my project") // space invalidates the name
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withCwd(t, dir, func() {
		got := suggestProfileNameFromCwd("local")
		if got != "" {
			t.Errorf("got %q, want empty (invalid name)", got)
		}
	})
}

// TestSuggestProfileNameFromCwdSkipMatchesCurrentName — when the cwd
// basename equals the colliding profile name, we don't suggest it
// (a clean recovery requires a different name).
func TestSuggestProfileNameFromCwdSkipMatchesCurrentName(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "local")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withCwd(t, dir, func() {
		got := suggestProfileNameFromCwd("local")
		if got != "" {
			t.Errorf("got %q, want empty (same as colliding name)", got)
		}
	})
}

// withCwd chdirs to dir for the duration of fn and restores the
// original working directory afterward, even on panic. Keeps the
// suggestion tests deterministic without leaking cwd state to other
// tests in the package.
func withCwd(t *testing.T, dir string, fn func()) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	defer func() {
		_ = os.Chdir(prev)
	}()
	fn()
}
