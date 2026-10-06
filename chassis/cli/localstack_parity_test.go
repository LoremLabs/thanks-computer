package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// The legacy* functions are frozen copies of the per-stack loops that
// buildStackFiles replaced (apply.go, dev.go, stacks_cmd.go/drift.go as of
// v0.2.43). TestBuildStackFilesParity proves the shared collector builds the
// same file set and upload set for each caller.

func legacyApplyStackFiles(t *testing.T, dir, stack string, ops []bundle.Op, allStacks map[string]bool) ([]client.StackFile, []casUpload) {
	t.Helper()
	stackDir := filepath.Join(dir, "OPS", stack)
	files := opsToFiles(ops)
	assets, err := collectFileAssets(stackDir)
	must(t, err)
	files = append(files, assets...)
	srcPacks, err := collectSourcePacks(stackDir)
	must(t, err)
	files = append(files, srcPacks...)
	outletFiles, err := collectOutletFiles(stackDir)
	must(t, err)
	files = append(files, outletFiles...)
	sandboxFiles, err := collectSandboxFiles(stackDir)
	must(t, err)
	files = append(files, sandboxFiles...)
	capFiles, err := collectCapFiles(stackDir)
	must(t, err)
	files = append(files, capFiles...)
	dsFiles, dsUploads, err := collectDatasetFiles(stackDir)
	must(t, err)
	files = append(files, dsFiles...)
	treeFiles, treeUploads, err := stackTreeRows(dir, stack, ops, allStacks)
	must(t, err)
	files = append(files, treeFiles...)
	return files, append(dsUploads, treeUploads...)
}

func legacyDevStackFiles(t *testing.T, dir, stack string, ops []bundle.Op, allStacks map[string]bool) ([]client.StackFile, []casUpload) {
	t.Helper()
	stackDir := filepath.Join(dir, "OPS", stack)
	files := opsToFiles(ops)
	assets, err := collectFileAssets(stackDir)
	must(t, err)
	files = append(files, assets...)
	packs, blobUploads, err := collectStorePacks(stackDir)
	must(t, err)
	files = append(files, packs...)
	srcPacks, err := collectSourcePacks(stackDir)
	must(t, err)
	files = append(files, srcPacks...)
	outletFiles, err := collectOutletFiles(stackDir)
	must(t, err)
	files = append(files, outletFiles...)
	sandboxFiles, err := collectSandboxFiles(stackDir)
	must(t, err)
	files = append(files, sandboxFiles...)
	capFiles, err := collectCapFiles(stackDir)
	must(t, err)
	files = append(files, capFiles...)
	dsFiles, dsUploads, err := collectDatasetFiles(stackDir)
	must(t, err)
	files = append(files, dsFiles...)
	treeFiles, treeUploads, err := stackTreeRows(dir, stack, ops, allStacks)
	must(t, err)
	files = append(files, treeFiles...)
	uploads := append(append(dsUploads, blobUploads...), treeUploads...)
	return files, uploads
}

func legacyLocalStackFiles(t *testing.T, dir, stack string, ops []bundle.Op) []client.StackFile {
	t.Helper()
	stackDir := filepath.Join(dir, "OPS", stack)
	files := opsToFiles(ops)
	assets, err := collectFileAssets(stackDir)
	must(t, err)
	files = append(files, assets...)
	srcPacks, err := collectSourcePacks(stackDir)
	must(t, err)
	files = append(files, srcPacks...)
	outletFiles, err := collectOutletFiles(stackDir)
	must(t, err)
	files = append(files, outletFiles...)
	sandboxFiles, err := collectSandboxFiles(stackDir)
	must(t, err)
	files = append(files, sandboxFiles...)
	capFiles, err := collectCapFiles(stackDir)
	must(t, err)
	files = append(files, capFiles...)
	dsFiles, _, err := collectDatasetFiles(stackDir)
	must(t, err)
	return append(files, dsFiles...)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sortedFiles(fs []client.StackFile) []client.StackFile {
	out := append([]client.StackFile(nil), fs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func allUploads(b *stackBuild) []casUpload {
	return append(append(append(append([]casUpload(nil), b.datasetUploads...), b.blobUploads...), b.sourceUploads...), b.treeUploads...)
}

func TestBuildStackFilesParity(t *testing.T) {
	root := t.TempDir()
	tree := map[string]string{
		"OPS/web/100/a.txcl":             `WHEN @src == "http" EMIT .a = 1`,
		"OPS/web/200_label/sub/b.txcl":   `EMIT .b = 2`,
		"OPS/web/300/tree.txcl":          "WITH cwd = \"$TXCO_STACK_DIR\", command = \"ls\"\nEXEC \"workspace://box/exec\"",
		"OPS/web/FILES/index.html":       "<!doctype html>hi",
		"OPS/web/FILES/img/x.bin":        "\x00\xff\xfe binary",
		"OPS/web/FILES/_mail/t.txt":      "private",
		"OPS/web/SOURCES/inbox.jsonl":    `{"kind":"imap"}`,
		"OPS/web/OUTLETS/db.yaml":        "driver: postgres\n",
		"OPS/web/SANDBOXES/tools.yaml":   "env: {}\n",
		"OPS/web/CAPS/search.yaml":       "entry: 100\n",
		"OPS/web/DATASETS/books.yaml":    "queries: {}\n",
		"OPS/web/DATASETS/books.sqlite":  "SQLite format 3\x00",
		"OPS/web/KV/seed.ndjson":         `{"k":"v"}`,
		"OPS/web/_mail/100/m.txcl":       `EMIT .m = 1`,
		"OPS/web/_mail/FILES/inner.html": "nested stack's own file",
	}
	for rel, body := range tree {
		p := filepath.Join(root, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	ops, diags, err := bundle.WalkDiag(root)
	must(t, err)
	if len(diags) > 0 {
		t.Fatalf("fixture diags: %+v", diags)
	}
	allStacks := stackNames(ops)
	stacks := groupOpsByStack(ops)

	for _, stack := range []string{"web", "web/_mail"} {
		sops := stacks[stack]
		if len(sops) == 0 {
			t.Fatalf("fixture has no ops for %s", stack)
		}

		wantFiles, wantUploads := legacyApplyStackFiles(t, root, stack, sops, allStacks)
		b, err := buildStackFiles(root, stack, sops, collectOpts{Derived: true, AllStacks: allStacks})
		must(t, err)
		if !reflect.DeepEqual(sortedFiles(b.Files), sortedFiles(wantFiles)) || !reflect.DeepEqual(allUploads(b), wantUploads) {
			t.Errorf("%s: apply set differs\n got %+v\nwant %+v", stack, sortedFiles(b.Files), sortedFiles(wantFiles))
		}
		if b.Hash() != localManifestHash(wantFiles) {
			t.Errorf("%s: apply hash differs", stack)
		}

		wantFiles, wantUploads = legacyDevStackFiles(t, root, stack, sops, allStacks)
		b, err = buildStackFiles(root, stack, sops, collectOpts{Data: true, Derived: true, AllStacks: allStacks})
		must(t, err)
		if !reflect.DeepEqual(sortedFiles(b.Files), sortedFiles(wantFiles)) || !reflect.DeepEqual(allUploads(b), wantUploads) {
			t.Errorf("%s: dev set differs\n got %+v\nwant %+v", stack, sortedFiles(b.Files), sortedFiles(wantFiles))
		}

		if stack == "web" {
			// The fixture must exercise every part, or parity proves little.
			has := map[string]bool{}
			for _, f := range b.Files {
				top, _, _ := cutFirst(f.Path)
				has[top] = true
			}
			for _, top := range []string{"FILES", "KV", "SOURCES", "OUTLETS", "SANDBOXES", "CAPS", "DATASETS", "STACKDIR"} {
				if !has[top] {
					t.Errorf("fixture never produced a %s/ row", top)
				}
			}
			if len(b.datasetUploads) == 0 || len(b.treeUploads) == 0 {
				t.Errorf("fixture produced no dataset or tree uploads")
			}
		}

		want := legacyLocalStackFiles(t, root, stack, sops)
		b, err = buildStackFiles(root, stack, sops, collectOpts{})
		must(t, err)
		if !reflect.DeepEqual(sortedFiles(b.Files), sortedFiles(want)) || len(allUploads(b)) != len(b.datasetUploads) {
			t.Errorf("%s: status/drift set differs", stack)
		}
	}
}

func cutFirst(p string) (string, string, bool) {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}
