package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// a minimal rule that strict-validates clean.
const okRule = "WHEN @src == \"http\"\n  EMIT @web.res.status = 200\n"

func runLintArgs(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runLint(args, &out, &errb)
	return code, out.String(), errb.String()
}

// TestLintClean: a well-formed tree passes with exit 0 and a ✓ summary.
func TestLintClean(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/a.txcl"), okRule)
	writeFile(t, filepath.Join(root, "OPS/site/200/b.txcl"), okRule)

	code, out, _ := runLintArgs(t, root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out=%q", code, out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "no issues") {
		t.Fatalf("missing clean summary: %q", out)
	}
}

// TestLintCollision: two files that flatten to the same stack/scope/name are a
// walk diagnostic → exit 1.
func TestLintCollision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/dup.txcl"), okRule)
	writeFile(t, filepath.Join(root, "OPS/site/100_alt/dup.txcl"), okRule)

	code, out, _ := runLintArgs(t, root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out=%q", code, out)
	}
	if !strings.Contains(out, "flatten to the same operation") {
		t.Fatalf("missing collision diagnostic: %q", out)
	}
}

// TestLintParseError: a rule with broken txcl is reported with its location and
// flips the exit to 1, even though the directory structure is valid.
func TestLintParseError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/ok.txcl"), okRule)
	writeFile(t, filepath.Join(root, "OPS/site/200/bad.txcl"), "WHEN @web.req.url.path == \"/oops\n")

	code, out, _ := runLintArgs(t, root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out=%q", code, out)
	}
	if !strings.Contains(out, "site/200/bad") {
		t.Fatalf("parse error missing op location: %q", out)
	}
}

// TestLintLoopWarning: an unconditional self-loop is reported as a warning
// (reusing apply's loop lint) but does NOT flip the exit code.
func TestLintLoopWarning(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/boot/0/loop.txcl"), "EMIT @goto = \"boot/0\"\n")

	code, out, _ := runLintArgs(t, root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warnings don't fail); out=%q", code, out)
	}
	if !strings.Contains(out, "⚠") || !strings.Contains(out, "warning(s)") {
		t.Fatalf("missing loop warning: %q", out)
	}
}

// TestLintList: --list prints the op graph (stack header + scopes in order).
func TestLintList(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/a.txcl"), okRule)
	writeFile(t, filepath.Join(root, "OPS/site/200/b.txcl"), okRule)

	code, out, _ := runLintArgs(t, "--list", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out=%q", code, out)
	}
	if !strings.Contains(out, "site") || !strings.Contains(out, "100  a") || !strings.Contains(out, "200  b") {
		t.Fatalf("op graph not printed: %q", out)
	}
}

// TestLintJSON: --json emits a parseable object whose ok/ops/graph reflect the
// tree; a broken rule flips ok=false and exit 1.
func TestLintJSON(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/a.txcl"), okRule)

	code, out, _ := runLintArgs(t, "--json", root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; out=%q", code, out)
	}
	var got struct {
		OK    bool `json:"ok"`
		Ops   int  `json:"ops"`
		Graph []struct {
			Stack string `json:"stack"`
			Scope int    `json:"scope"`
			Name  string `json:"name"`
		} `json:"graph"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !got.OK || got.Ops != 1 || len(got.Graph) != 1 || got.Graph[0].Name != "a" {
		t.Fatalf("unexpected JSON payload: %+v", got)
	}

	// now a broken rule → ok=false, exit 1, still valid JSON.
	writeFile(t, filepath.Join(root, "OPS/site/200/bad.txcl"), "WHEN @web.req.url.path == \"/oops\n")
	code, out, _ = runLintArgs(t, "--json", root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; out=%q", code, out)
	}
	got.OK = true
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON on error path: %v\n%s", err, out)
	}
	if got.OK {
		t.Fatalf("expected ok=false on parse error, got %q", out)
	}
}
