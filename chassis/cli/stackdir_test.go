package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/stackdir"
)

func writeTreeFile(t *testing.T, root, rel string, mode os.FileMode, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestStackTreeRows(t *testing.T) {
	dir := t.TempDir()
	ops := filepath.Join(dir, "OPS")
	for rel, content := range map[string]string{
		"game/2100_SETUP/setup.txcl":  `WHEN 1 == 1 WITH cwd = "$TXCO_STACK_DIR", args = ["python3", "2100_SETUP/race.py"] EXEC "workspace://b/exec"`,
		"game/2100_SETUP/race.py":     "print(1)\n",
		"game/_lib/util.py":           "x = 1\n", // a helper directory, not a stack
		"game/FILES/index.html":       "<p>served</p>",
		"game/CAPS/game.play.yaml":    "name: game.play\n",
		"game/.env":                   "SECRET=1\n",
		"game/2100_SETUP/.cache/x":    "junk",
		"game/_ws/0100_MSG/msg.txcl":  `WHEN 1 == 1 EMIT .x = 1`, // a nested stack
		"game/_ws/0100_MSG/helper.py": "nested\n",
		"plain/0100_A/a.txcl":         `WHEN 1 == 1 EMIT .a = 1`,
		"plain/0100_A/tool.py":        "unused\n",
	} {
		writeTreeFile(t, ops, rel, 0o644, content)
	}
	writeTreeFile(t, ops, "game/bin/run", 0o755, "#!/bin/sh\n")

	walked, err := bundle.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := stackNames(walked)
	stacks := groupOpsByStack(walked)
	if !all["game/_ws"] {
		t.Fatalf("walk did not find the nested stack: %v", all)
	}

	rows, uploads, err := stackTreeRows(dir, "game", stacks["game"], all)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(uploads) != 1 || !stackdir.IsPath(rows[0].Path) || rows[0].Encoding != "cas" {
		t.Fatalf("rows = %+v, uploads = %+v", rows, uploads)
	}
	digest := stackdir.DigestFromPath(rows[0].Path)
	if digest != rows[0].ContentHash || uploads[0].Hash != digest {
		t.Fatalf("row hash %q, path digest %q, upload %q", rows[0].ContentHash, digest, uploads[0].Hash)
	}
	data, err := os.ReadFile(uploads[0].LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	execs := map[string]bool{}
	if err := stackdir.Unpack(data, func(f stackdir.File) error {
		paths = append(paths, f.Path)
		execs[f.Path] = f.Exec
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	want := []string{"2100_SETUP/race.py", "2100_SETUP/setup.txcl", "_lib/util.py", "bin/run"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("tree = %v, want %v", paths, want)
	}
	if !execs["bin/run"] || execs["2100_SETUP/race.py"] {
		t.Fatalf("executable bits = %v", execs)
	}

	// The same files pack the same: a re-apply mints nothing new.
	again, _, err := stackTreeRows(dir, "game", stacks["game"], all)
	if err != nil || again[0].ContentHash != digest {
		t.Fatalf("second pack = %+v, %v", again, err)
	}

	// A stack that never names the tree carries no row.
	if rows, uploads, err := stackTreeRows(dir, "plain", stacks["plain"], all); err != nil || rows != nil || uploads != nil {
		t.Fatalf("plain: %+v %+v %v", rows, uploads, err)
	}
}

func TestStackTreeCaps(t *testing.T) {
	dir := t.TempDir()
	ops := filepath.Join(dir, "OPS")
	writeTreeFile(t, ops, "big/0100_A/a.txcl", 0o644, `WHEN 1 == 1 WITH cwd = "$TXCO_STACK_DIR", command = "x" EXEC "workspace://w/exec"`)
	writeTreeFile(t, ops, "big/0100_A/huge.bin", 0o644, string(bytes.Repeat([]byte("x"), stackdir.MaxFileBytes+1)))
	walked, err := bundle.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = stackTreeRows(dir, "big", groupOpsByStack(walked)["big"], stackNames(walked))
	if err == nil || !strings.Contains(err.Error(), "0100_A/huge.bin") {
		t.Fatalf("an oversize file was packed: %v", err)
	}
}
