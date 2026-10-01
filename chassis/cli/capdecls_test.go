package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
)

func TestCollectCapFiles(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "OPS/loop")
	if files, err := collectCapFiles(stackDir); err != nil || files != nil {
		t.Fatalf("absent CAPS/: %v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "CAPS/mail.send.yaml"), "entry: 7000\n")
	writeFile(t, filepath.Join(stackDir, "CAPS/ai.chat.yaml"), "entry: 7000\n")
	writeFile(t, filepath.Join(stackDir, "CAPS/.hidden.yaml"), "x")
	files, err := collectCapFiles(stackDir)
	if err != nil || len(files) != 2 || files[0].Path != "CAPS/ai.chat.yaml" || files[1].Path != "CAPS/mail.send.yaml" || files[0].Content == "" || files[0].ContentHash == "" {
		t.Fatalf("collect: %+v %v", files, err)
	}
	writeFile(t, filepath.Join(stackDir, "CAPS/Bad.Name.yaml"), "entry: 7000\n")
	if _, err := collectCapFiles(stackDir); err == nil || !strings.Contains(err.Error(), "Bad.Name.yaml") {
		t.Fatalf("bad name must fail: %v", err)
	}
}

func TestCheckCapDecls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/loop/CAPS/mail.send.yaml"), "description: Send a message.\nentry: 7000\ninput:\n  to:\n    required: true\n")
	writeFile(t, filepath.Join(root, "OPS/loop/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	writeFile(t, filepath.Join(root, "OPS/loop/7000/run.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	writeFile(t, filepath.Join(root, "OPS/other/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	ops, _, err := bundle.WalkDiag(root)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := checkCapDecls(ops, root); len(msgs) != 0 {
		t.Fatalf("clean: %v", msgs)
	}
	// A declaration that does not parse, an entry that names no scope of
	// its stack, and a name a second stack of the workspace declares: one
	// message each, against the file.
	writeFile(t, filepath.Join(root, "OPS/loop/CAPS/broken.yaml"), "entry: 7000\noutput: {}\n")
	writeFile(t, filepath.Join(root, "OPS/loop/CAPS/card.note.yaml"), "entry: 7100\n")
	writeFile(t, filepath.Join(root, "OPS/other/CAPS/mail.send.yaml"), "entry: 100\n")
	msgs := checkCapDecls(ops, root)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d:\n%s", len(msgs), strings.Join(msgs, "\n"))
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{
		"loop/CAPS/broken.yaml: capability declaration:",
		"field output not found",
		"loop/CAPS/card.note.yaml: capability declaration: entry 7100 names no scope of this stack",
		`other/CAPS/mail.send.yaml: capability "mail.send" is already declared by stack "loop"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestLintCapErrorFlipsExit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/loop/CAPS/mail.send.yaml"), "entry: 100\n")
	writeFile(t, filepath.Join(root, "OPS/loop/100/plain.txcl"), `EXEC "txco://copy" WITH from = "a", to = "b"`+"\n")
	var stdout, stderr bytes.Buffer
	if code := runLint([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean workspace: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	writeFile(t, filepath.Join(root, "OPS/loop/CAPS/card.note.yaml"), "entry: 7000\n")
	stdout.Reset()
	stderr.Reset()
	if code := runLint([]string{root}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "cap: ") || !strings.Contains(stdout.String(), "names no scope") {
		t.Fatalf("an entry with no scope must fail lint: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}
