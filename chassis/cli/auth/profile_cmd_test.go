package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedMeta is a small helper for the profile-verb tests — drops a
// realistic meta file under $TXCO_HOME/keys/<name>.meta.json so the
// commands have something to act on. Returns the meta path.
func seedMeta(t *testing.T, name string, m Meta) string {
	t.Helper()
	mp, err := MetaPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
		t.Fatal(err)
	}
	if m.EnrolledAt.IsZero() {
		m.EnrolledAt = time.Now().UTC()
	}
	if err := SaveMeta(mp, m); err != nil {
		t.Fatal(err)
	}
	return mp
}

// TestRunProfilesEmpty — no profiles configured yet → friendly
// "no profiles" message, exit 0.
func TestRunProfilesEmpty(t *testing.T) {
	withHome(t)
	var stdout, stderr bytes.Buffer
	if code := runProfiles(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no profiles") {
		t.Errorf("expected friendly empty-state message; got %q", stdout.String())
	}
}

// TestRunProfilesListsAndMarksActive — two profiles, one active.
// Output table includes both, with the active one marked with `*`.
func TestRunProfilesListsAndMarksActive(t *testing.T) {
	withHome(t)
	seedMeta(t, "local", Meta{ActorID: "actor_local", KeyID: "key_local", ChassisURL: "http://x"})
	seedMeta(t, "work", Meta{ActorID: "actor_work", KeyID: "key_work", ChassisURL: "http://y"})
	if err := WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runProfiles(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "work") || !strings.Contains(out, "local") {
		t.Errorf("listing should include both profiles; got %q", out)
	}
	// Active row begins with "*"; the inactive row begins with a
	// space. Check that the line containing "work" has the asterisk.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "work") && !strings.HasPrefix(line, "*") {
			t.Errorf("expected work line to start with *; got %q", line)
		}
	}
}

// TestRunProfileUseHappy — flip the active pointer; future reads
// see the change.
func TestRunProfileUseHappy(t *testing.T) {
	withHome(t)
	seedMeta(t, "work", Meta{ActorID: "actor_work", KeyID: "key_work", ChassisURL: "http://x"})
	var stdout, stderr bytes.Buffer
	if code := runProfileUse([]string{"work"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if got, _ := ReadActiveProfile(); got != "work" {
		t.Errorf("active=%q, want work", got)
	}
}

// TestRunProfileUseRejectsUnknown — refusing to activate a profile
// that doesn't exist saves the user a confusing "no signing key"
// error on the next command.
func TestRunProfileUseRejectsUnknown(t *testing.T) {
	withHome(t)
	var stdout, stderr bytes.Buffer
	if code := runProfileUse([]string{"ghost"}, &stdout, &stderr); code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("expected 'not found' message; got %q", stderr.String())
	}
	// Active pointer must be unchanged.
	if got, _ := ReadActiveProfile(); got != DefaultProfile {
		t.Errorf("active changed to %q; should remain %q", got, DefaultProfile)
	}
}

// TestRunProfileShowActive — no arg shows the active profile.
func TestRunProfileShowActive(t *testing.T) {
	withHome(t)
	seedMeta(t, "work", Meta{
		ActorID:    "actor_work",
		KeyID:      "key_work",
		ChassisURL: "http://example",
		Label:      "matt@laptop",
	})
	_ = WriteActiveProfile("work")

	var stdout, stderr bytes.Buffer
	if code := runProfileShow(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"profile:      work", "status:       active", "actor_work", "matt@laptop"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output lacks %q; got:\n%s", want, out)
		}
	}
}

// TestRunProfileShowLoggedOut — no active profile (logout state) →
// "no active profile" message, exit 0.
func TestRunProfileShowLoggedOut(t *testing.T) {
	withHome(t)
	_ = WriteActiveProfile(ActiveNone)
	var stdout, stderr bytes.Buffer
	if code := runProfileShow(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no active profile") {
		t.Errorf("expected 'no active profile' message; got %q", stdout.String())
	}
}

// TestRunProfileRemoveKeepsKeyFile — the core safety property.
// Remove a profile via the CLI; the meta file is gone but a sibling
// key file (or external --ssh-key target) is untouched.
func TestRunProfileRemoveKeepsKeyFile(t *testing.T) {
	withHome(t)
	mp := seedMeta(t, "work", Meta{
		ActorID:    "actor_work",
		KeyID:      "key_work",
		ChassisURL: "http://x",
		KeySource:  SourceFile,
		KeyPath:    "/external/keep-me.ed25519",
	})
	// Stage a placeholder where the meta says the key lives.
	_ = os.MkdirAll("/tmp/txco-keep-me", 0o700)
	keepPath := "/tmp/txco-keep-me/keep.ed25519"
	if err := os.WriteFile(keepPath, []byte("key data"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(keepPath) })

	// Re-seed meta with the actual placeholder path so the remove
	// command can confidently echo "will NOT touch <keepPath>".
	_ = SaveMeta(mp, Meta{
		ActorID: "actor_work", KeyID: "key_work",
		ChassisURL: "http://x", KeySource: SourceFile, KeyPath: keepPath,
	})

	var stdout, stderr bytes.Buffer
	if code := runProfileRemove([]string{"-y", "work"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(mp); err == nil {
		t.Errorf("meta file should have been removed; still at %q", mp)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Errorf("key file should be untouched at %q: %v", keepPath, err)
	}
	if !strings.Contains(stderr.String(), "will NOT touch") {
		t.Errorf("stderr should explicitly mention NOT touching the key; got %q", stderr.String())
	}
}

// TestRunProfileRemoveActiveClearsActive — removing the currently-
// active profile downgrades the active pointer to ActiveNone (so
// the next command doesn't keep pointing at a missing meta).
func TestRunProfileRemoveActiveClearsActive(t *testing.T) {
	withHome(t)
	seedMeta(t, "work", Meta{ActorID: "actor_work", KeyID: "key_work", ChassisURL: "http://x"})
	_ = WriteActiveProfile("work")
	var stdout, stderr bytes.Buffer
	if code := runProfileRemove([]string{"-y", "work"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if got, _ := ReadActiveProfile(); got != ActiveNone {
		t.Errorf("active=%q, want %q after removing active profile", got, ActiveNone)
	}
}

// TestRunLogoutSetsActiveNone — `txco auth logout` flips the active
// pointer to ActiveNone but doesn't remove any files.
func TestRunLogoutSetsActiveNone(t *testing.T) {
	withHome(t)
	mp := seedMeta(t, "local", Meta{ActorID: "actor_local", KeyID: "key_local", ChassisURL: "http://x"})
	_ = WriteActiveProfile("local")

	var stdout, stderr bytes.Buffer
	if code := runLogout(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if got, _ := ReadActiveProfile(); got != ActiveNone {
		t.Errorf("active=%q, want %q", got, ActiveNone)
	}
	// Meta file MUST still exist — logout is soft.
	if _, err := os.Stat(mp); err != nil {
		t.Errorf("logout should not have touched the meta; %v", err)
	}
}
