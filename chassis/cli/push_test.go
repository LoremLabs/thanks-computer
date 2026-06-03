package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStackFixture lays down OPS/<stack>/<scope>/<name>.txcl under root so
// bundle.Walk discovers a single-resonator stack.
func writeStackFixture(t *testing.T, root, stack, scope, name, txcl string) {
	t.Helper()
	dir := filepath.Join(root, "OPS", stack, scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".txcl"), []byte(txcl), 0o644); err != nil {
		t.Fatalf("write txcl: %v", err)
	}
}

func TestRunPushMissingStack(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runPush(nil, &out, &errb); code != 2 {
		t.Fatalf("exit=%d, want 2 for missing <stack>", code)
	}
	if !strings.Contains(errb.String(), "missing <stack>") {
		t.Errorf("stderr should explain the missing arg: %q", errb.String())
	}
}

func TestRunPushStackNotFound(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)

	var out, errb bytes.Buffer
	// Ask to push "web" but only "api" exists — should refuse and list it.
	code := runPush([]string{"web", root}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), `stack "web" not found`) ||
		!strings.Contains(errb.String(), "available: api") {
		t.Errorf("stderr should name the missing stack + list available: %q", errb.String())
	}
}

// TestRunPushHonorsFlagsAfterPositional proves push parses flags that trail
// the <stack> positional (the bug class that motivated the pflag switch):
// `txco push api <dir> --addr <x>` must target <x>, not localhost.
func TestRunPushHonorsFlagsAfterPositional(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)

	var out, errb bytes.Buffer
	code := runPush([]string{"api", root, "--addr", "http://order-check.invalid:9"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (unreachable); stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "order-check.invalid") {
		t.Fatalf("--addr after the <stack>/<dir> positionals was ignored: %q", errb.String())
	}
}
