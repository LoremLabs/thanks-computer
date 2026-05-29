package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretsInitHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	var stdout, stderr bytes.Buffer
	rc := runSecretsInit([]string{"--path", path}, strings.NewReader(""), &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstderr: %s", rc, stderr.String())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 32 {
		t.Errorf("size = %d, want 32", info.Size())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
	if !strings.Contains(stdout.String(), path) {
		t.Errorf("stdout missing path: %s", stdout.String())
	}
}

func TestSecretsInitFallsBackToEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	t.Setenv("TXCO_SECRET_MASTER_KEY", path)

	var stdout, stderr bytes.Buffer
	rc := runSecretsInit(nil, strings.NewReader(""), &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstderr: %s", rc, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected key at %s: %v", path, err)
	}
}

func TestSecretsInitRequiresPath(t *testing.T) {
	t.Setenv("TXCO_SECRET_MASTER_KEY", "")
	var stdout, stderr bytes.Buffer
	rc := runSecretsInit(nil, strings.NewReader(""), &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (missing --path)")
	}
	if !strings.Contains(stderr.String(), "--path") {
		t.Errorf("stderr should mention --path requirement: %s", stderr.String())
	}
}

func TestSecretsInitRefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	// First mint succeeds.
	var stdout, stderr bytes.Buffer
	if rc := runSecretsInit([]string{"--path", path}, strings.NewReader(""), &stdout, &stderr); rc != 0 {
		t.Fatalf("first mint rc = %d: %s", rc, stderr.String())
	}

	// Read existing key bytes so we can verify they didn't change.
	originalBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Second invocation without --force must refuse.
	stdout.Reset()
	stderr.Reset()
	rc := runSecretsInit([]string{"--path", path}, strings.NewReader(""), &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (refuse overwrite)")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr should suggest --force: %s", stderr.String())
	}

	// And confirm the existing file is untouched.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(originalBytes, after) {
		t.Errorf("existing key bytes were modified despite refuse-overwrite")
	}
}

func TestSecretsInitForceWithConfirmation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	// First mint.
	var stdout, stderr bytes.Buffer
	if rc := runSecretsInit([]string{"--path", path}, strings.NewReader(""), &stdout, &stderr); rc != 0 {
		t.Fatalf("first mint rc = %d", rc)
	}
	originalBytes, _ := os.ReadFile(path)

	// --force + matching "overwrite" confirmation.
	stdout.Reset()
	stderr.Reset()
	rc := runSecretsInit([]string{"--path", path, "--force"}, strings.NewReader("overwrite\n"), &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstderr: %s", rc, stderr.String())
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if bytes.Equal(originalBytes, after) {
		t.Errorf("--force overwrite did not change the key bytes")
	}
	if len(after) != 32 {
		t.Errorf("new key size = %d, want 32", len(after))
	}
}

func TestSecretsInitForceWrongConfirmationAborts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	var stdout, stderr bytes.Buffer
	if rc := runSecretsInit([]string{"--path", path}, strings.NewReader(""), &stdout, &stderr); rc != 0 {
		t.Fatalf("first mint rc = %d", rc)
	}
	originalBytes, _ := os.ReadFile(path)

	// --force but user types "no" — must abort, key untouched.
	stdout.Reset()
	stderr.Reset()
	rc := runSecretsInit([]string{"--path", path, "--force"}, strings.NewReader("no\n"), &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (aborted)")
	}
	if !strings.Contains(stdout.String(), "aborted") {
		t.Errorf("stdout should say 'aborted': %s", stdout.String())
	}

	after, _ := os.ReadFile(path)
	if !bytes.Equal(originalBytes, after) {
		t.Errorf("aborted --force should not modify key bytes")
	}
}

func TestSecretsCmdDispatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	t.Setenv("TXCO_SECRET_MASTER_KEY", path)

	var stdout, stderr bytes.Buffer
	rc := runSecretsCmd([]string{"init"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("dispatch 'init' rc = %d: %s", rc, stderr.String())
	}
}

func TestSecretsCmdUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runSecretsCmd([]string{"frobnicate"}, &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (unknown subcommand)")
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr should mention unknown subcommand: %s", stderr.String())
	}
}

func TestSecretsCmdNoArgsPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runSecretsCmd(nil, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if !strings.Contains(stdout.String(), "txco auth secrets") {
		t.Errorf("stdout should include usage banner: %s", stdout.String())
	}
}

// TestSecretsInitRejectsDirectoryPath pins the UX fix: when --path
// points at a directory, the CLI must reject loud with a useful
// hint — NOT report "already exists; pass --force" (which would
// confuse the operator AND, if they passed --force, would delete
// the directory and write a file at the same name).
func TestSecretsInitRejectsDirectoryPath(t *testing.T) {
	dir := t.TempDir() // dir itself is a directory; --path it directly

	var stdout, stderr bytes.Buffer
	rc := runSecretsInit([]string{"--path", dir}, strings.NewReader(""), &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (directory path)")
	}
	if !strings.Contains(stderr.String(), "is a directory") {
		t.Errorf("stderr should call out the directory: %s", stderr.String())
	}
	// And must NOT suggest --force (would delete the dir).
	if strings.Contains(stderr.String(), "--force") {
		t.Errorf("error must NOT suggest --force for directories; got: %s", stderr.String())
	}
	// Should suggest a file-path form so operator knows what to type next.
	if !strings.Contains(stderr.String(), "txco-master.key") {
		t.Errorf("error should suggest a filename like 'txco-master.key': %s", stderr.String())
	}
}

// TestSecretsInitWithDirectoryAndForceStillRejects pins the most
// dangerous variant: --path is a directory AND --force is passed.
// The CLI must STILL refuse — never delete a directory just because
// the operator asked to "force" a file write at the same name.
func TestSecretsInitWithDirectoryAndForceStillRejects(t *testing.T) {
	dir := t.TempDir()

	var stdout, stderr bytes.Buffer
	rc := runSecretsInit([]string{"--path", dir, "--force"},
		strings.NewReader("overwrite\n"), &stdout, &stderr)
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (directory + --force must still refuse)")
	}
	if !strings.Contains(stderr.String(), "is a directory") {
		t.Errorf("stderr should call out the directory: %s", stderr.String())
	}
	// And the directory must still exist (not deleted by accident).
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory was deleted under --force: %v", err)
	}
}

func TestSecretsCmdHelp(t *testing.T) {
	for _, h := range []string{"help", "-h", "--help"} {
		t.Run(h, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := runSecretsCmd([]string{h}, &stdout, &stderr)
			if rc != 0 {
				t.Errorf("rc = %d, want 0", rc)
			}
			if !strings.Contains(stdout.String(), "txco auth secrets") {
				t.Errorf("stdout should include usage banner: %s", stdout.String())
			}
		})
	}
}
