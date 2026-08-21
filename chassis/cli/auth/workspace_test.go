package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkProfile registers a keyless profile (meta only — the `txco dev` shape)
// under the test's TXCO_HOME.
func mkProfile(t *testing.T, name string) {
	t.Helper()
	mp, err := MetaPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveMeta(mp, Meta{ChassisURL: "http://localhost:8081"}); err != nil {
		t.Fatal(err)
	}
}

// chdir switches CWD for the test and restores it after.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestWorkspaceProfileResolves(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_PROFILE", "")
	mkProfile(t, "dev")

	ws := t.TempDir()
	yaml := "target: dev\ntargets:\n  dev:\n    chassis: http://localhost:8081\n    profile: dev\n"
	if err := os.WriteFile(filepath.Join(ws, "txco.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// … and from a NESTED directory, proving the upward walk.
	nested := filepath.Join(ws, "OPS", "pony", "0100_REQ")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, nested)

	name, source, err := WorkspaceProfile()
	if err != nil {
		t.Fatalf("WorkspaceProfile: %v", err)
	}
	if name != "dev" {
		t.Fatalf("name = %q, want dev", name)
	}
	if !strings.Contains(source, "txco.yaml") || !strings.Contains(source, `"dev"`) {
		t.Fatalf("source = %q, want txco.yaml + target name", source)
	}

	// The full chain agrees, and beats an active profile pointing elsewhere.
	if err := WriteActiveProfile("prod-elsewhere"); err != nil {
		t.Fatal(err)
	}
	got, gotSrc, err := ResolveProfileSource("")
	if err != nil {
		t.Fatalf("ResolveProfileSource: %v", err)
	}
	if got != "dev" {
		t.Fatalf("resolved %q, want dev (workspace must beat active)", got)
	}
	if !strings.Contains(gotSrc, "txco.yaml") {
		t.Fatalf("source = %q, want workspace", gotSrc)
	}

	// Explicit flag and TXCO_PROFILE still beat the workspace.
	if got, _, _ := ResolveProfileSource("flagged"); got != "flagged" {
		t.Fatalf("flag must win, got %q", got)
	}
	t.Setenv("TXCO_PROFILE", "enved")
	if got, _, _ := ResolveProfileSource(""); got != "enved" {
		t.Fatalf("TXCO_PROFILE must beat workspace, got %q", got)
	}
}

func TestWorkspaceProfileLoudMiss(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_PROFILE", "")

	ws := t.TempDir()
	yaml := "target: dev\ntargets:\n  dev:\n    profile: ghost\n"
	if err := os.WriteFile(filepath.Join(ws, "txco.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, ws)

	// A binding to a nonexistent profile must ERROR, not silently fall
	// back to the active profile — that fallback would re-create the
	// wrong-chassis trap this feature exists to close.
	if _, _, err := WorkspaceProfile(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want loud miss naming the profile, got %v", err)
	}
	if _, _, err := ResolveProfileSource(""); err == nil {
		t.Fatal("ResolveProfileSource must propagate the loud miss")
	}
}

func TestWorkspaceProfileNoOpinion(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_PROFILE", "")

	// No txco.yaml at all → no opinion.
	empty := t.TempDir()
	chdir(t, empty)
	if name, _, err := WorkspaceProfile(); err != nil || name != "" {
		t.Fatalf("no yaml: want no opinion, got %q err %v", name, err)
	}

	// txco.yaml present but no profile binding → no opinion (a workspace
	// that declares nothing keeps today's behavior exactly).
	ws := t.TempDir()
	yaml := "target: dev\ntargets:\n  dev:\n    chassis: http://localhost:8081\n"
	if err := os.WriteFile(filepath.Join(ws, "txco.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, ws)
	if name, _, err := WorkspaceProfile(); err != nil || name != "" {
		t.Fatalf("unbound yaml: want no opinion, got %q err %v", name, err)
	}

	// Chain falls through to the active profile as before.
	if err := WriteActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	name, source, err := ResolveProfileSource("")
	if err != nil || name != "work" || source != "active profile" {
		t.Fatalf("fallback: got %q/%q err %v, want work/active profile", name, source, err)
	}
}
