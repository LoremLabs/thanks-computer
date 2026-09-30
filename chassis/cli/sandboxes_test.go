package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
)

func TestCollectSandboxFiles(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "OPS/site")
	if files, err := collectSandboxFiles(stackDir); err != nil || files != nil {
		t.Fatalf("absent SANDBOXES/: %v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "SANDBOXES/github.yaml"), "env:\n  GH_TOKEN: secret:GITHUB_PAT\n")
	writeFile(t, filepath.Join(stackDir, "SANDBOXES/.hidden.yaml"), "x")
	files, err := collectSandboxFiles(stackDir)
	if err != nil || len(files) != 1 || files[0].Path != "SANDBOXES/github.yaml" || files[0].Content == "" || files[0].ContentHash == "" {
		t.Fatalf("collect: %+v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "SANDBOXES/Bad.yaml"), "env:\n  A: secret:B\n")
	if _, err := collectSandboxFiles(stackDir); err == nil || !strings.Contains(err.Error(), "Bad.yaml") {
		t.Fatalf("bad name must fail: %v", err)
	}
}

func TestCheckSandboxDecls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/SANDBOXES/github.yaml"), "env:\n  GH_TOKEN: secret:GITHUB_PAT\n")
	writeFile(t, filepath.Join(root, "OPS/site/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	writeFile(t, filepath.Join(root, "OPS/other/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	ops, _, err := bundle.WalkDiag(root)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := checkSandboxDecls(ops, root); len(msgs) != 0 {
		t.Fatalf("clean: %v", msgs)
	}
	// Every declaration is parsed, whether or not an op names it: a broken
	// one is reported once, against its file.
	writeFile(t, filepath.Join(root, "OPS/site/SANDBOXES/broken.yaml"), "env:\n  A: secret:B\nnetwork: [github.com]\n")
	writeFile(t, filepath.Join(root, "OPS/other/SANDBOXES/empty.yaml"), "description: nothing\n")
	msgs := checkSandboxDecls(ops, root)
	if len(msgs) != 2 || !strings.Contains(msgs[0], "other/SANDBOXES/empty.yaml") || !strings.Contains(msgs[0], "env is required") ||
		!strings.Contains(msgs[1], "site/SANDBOXES/broken.yaml") || !strings.Contains(msgs[1], "field network not found") {
		t.Fatalf("broken declarations: %s", strings.Join(msgs, "\n"))
	}
}

func TestLintSandboxErrorFlipsExit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/SANDBOXES/github.yaml"), "env:\n  GH_TOKEN: secret:GITHUB_PAT\n")
	writeFile(t, filepath.Join(root, "OPS/site/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	var stdout, stderr bytes.Buffer
	if code := runLint([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean workspace: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	writeFile(t, filepath.Join(root, "OPS/site/SANDBOXES/bad.yaml"), "env:\n  TXCO_RUN: secret:X\n")
	stdout.Reset()
	stderr.Reset()
	if code := runLint([]string{root}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "sandbox: ") || !strings.Contains(stdout.String(), "TXCO_") {
		t.Fatalf("a reserved variable must fail lint: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}
