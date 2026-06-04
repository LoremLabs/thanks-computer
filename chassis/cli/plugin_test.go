package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExec creates an executable shell script at path. POSIX-only fixture.
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A `txco-<name>` executable on $PATH handles `txco <name> ...`: it receives the
// remaining args, its stdout passes through, and its exit code propagates.
func TestExecPluginViaPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based plugin fixture is POSIX-only")
	}
	pathDir := t.TempDir()
	t.Setenv("TXCO_HOME", t.TempDir()) // hermetic: empty plugins dir
	t.Setenv("PATH", pathDir)
	writeExec(t, filepath.Join(pathDir, "txco-greet"), "#!/bin/sh\nprintf 'greet:%s' \"$1\"\nexit 7\n")

	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "greet", "alice"}, &out, &errb)
	if !ok {
		t.Fatalf("Dispatch ok=false; want the plugin to handle 'greet'")
	}
	if status != 7 {
		t.Errorf("status=%d, want 7 (plugin exit code propagated)", status)
	}
	if got := out.String(); !strings.Contains(got, "greet:alice") {
		t.Errorf("stdout=%q, want it to contain greet:alice", got)
	}
}

// The plugins dir ($TXCO_HOME/plugins) takes precedence over $PATH.
func TestFindPluginPrefersPluginsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based plugin fixture is POSIX-only")
	}
	home := t.TempDir()
	pathDir := t.TempDir()
	t.Setenv("TXCO_HOME", home)
	t.Setenv("PATH", pathDir)
	dirPlugin := filepath.Join(home, "plugins", "txco-foo")
	writeExec(t, dirPlugin, "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(pathDir, "txco-foo"), "#!/bin/sh\nexit 0\n") // also on PATH

	got, ok := findPlugin("foo")
	if !ok {
		t.Fatal("findPlugin(foo) not found")
	}
	if got != dirPlugin {
		t.Errorf("findPlugin(foo) = %q, want the plugins-dir copy %q", got, dirPlugin)
	}
}

// With no matching plugin anywhere, an unknown subcommand stays an error.
func TestExecPluginUnknownFallsThrough(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir()) // empty plugins dir
	t.Setenv("PATH", t.TempDir())      // empty PATH
	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "definitelynotaplugin"}, &out, &errb)
	if !ok || status != 2 {
		t.Errorf("status=%d ok=%v, want 2/true (unknown subcommand)", status, ok)
	}
	if !strings.Contains(errb.String(), "unknown subcommand") {
		t.Errorf("stderr=%q, want the unknown-subcommand message", errb.String())
	}
}

// `txco plugin list` reports discovered plugins by name and path.
func TestPluginList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based plugin fixture is POSIX-only")
	}
	home := t.TempDir()
	t.Setenv("TXCO_HOME", home)
	t.Setenv("PATH", t.TempDir()) // empty: only the plugins-dir entry should show
	dirPlugin := filepath.Join(home, "plugins", "txco-credit")
	writeExec(t, dirPlugin, "#!/bin/sh\nexit 0\n")

	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "plugin", "list"}, &out, &errb)
	if !ok || status != 0 {
		t.Fatalf("plugin list status=%d ok=%v, want 0/true", status, ok)
	}
	got := out.String()
	if !strings.Contains(got, "credit") || !strings.Contains(got, dirPlugin) {
		t.Errorf("plugin list output = %q, want it to list credit -> %s", got, dirPlugin)
	}
}

// `txco plugin list` with nothing installed prints a friendly hint, not an error.
func TestPluginListEmpty(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "plugin", "list"}, &out, &errb)
	if !ok || status != 0 {
		t.Fatalf("status=%d ok=%v, want 0/true", status, ok)
	}
	if !strings.Contains(out.String(), "No txco plugins found") {
		t.Errorf("output = %q, want the empty-state hint", out.String())
	}
}
