package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/stackdir"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// unlockTrees lets the test's temp dir be removed: placed trees are read-only.
func unlockTrees(t *testing.T, p *Provider) {
	t.Cleanup(func() { removeTree(filepath.Join(p.Root(), treesDir)) })
}

func testTree(t *testing.T, files ...stackdir.File) (workspace.Tree, *atomic.Int32) {
	t.Helper()
	data, digest, err := stackdir.Pack(files)
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	return workspace.Tree{Digest: digest, Open: func(context.Context) ([]byte, error) {
		reads.Add(1)
		return data, nil
	}}, &reads
}

func TestPlaceTree(t *testing.T) {
	p, c, _ := newComputer(t)
	unlockTrees(t, p)
	tree, reads := testTree(t,
		stackdir.File{Path: "2100_SETUP/race.py", Content: []byte("print('hi')\n")},
		stackdir.File{Path: "bin/run", Exec: true, Content: []byte("#!/bin/sh\necho ran\n")},
	)
	th := c.(workspace.TreeHolder)
	dir, err := th.PlaceTree(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(p.Root(), treesDir, tree.Digest); dir != want {
		t.Fatalf("placed at %q, want %q", dir, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "2100_SETUP", "race.py")); err != nil || string(b) != "print('hi')\n" {
		t.Fatalf("race.py = %q, %v", b, err)
	}
	for rel, want := range map[string]os.FileMode{"2100_SETUP/race.py": 0o444, "bin/run": 0o555, "bin": 0o555, ".": 0o555} {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", rel, got, want)
		}
	}
	// A second placement of the same digest reads nothing.
	if again, err := th.PlaceTree(context.Background(), tree); err != nil || again != dir || reads.Load() != 1 {
		t.Fatalf("second placement: %q, %v, reads %d", again, err, reads.Load())
	}
	// Another workspace on this chassis shares the one copy.
	h2, _ := p.Create(context.Background(), workspace.Spec{Tenant: "acme", Stack: "other", Name: "tools"})
	c2, _ := p.Wake(context.Background(), h2)
	if shared, err := c2.(workspace.TreeHolder).PlaceTree(context.Background(), tree); err != nil || shared != dir {
		t.Fatalf("shared placement: %q, %v", shared, err)
	}
}

func TestPlaceTreeConcurrent(t *testing.T) {
	p, c, _ := newComputer(t)
	unlockTrees(t, p)
	tree, _ := testTree(t, stackdir.File{Path: "a/b.txt", Content: []byte("x")})
	th := c.(workspace.TreeHolder)
	var wg sync.WaitGroup
	dirs := make([]string, 8)
	errs := make([]error, 8)
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dirs[i], errs[i] = th.PlaceTree(context.Background(), tree)
		}(i)
	}
	wg.Wait()
	for i := range dirs {
		if errs[i] != nil || dirs[i] != dirs[0] {
			t.Fatalf("placement %d: %q, %v", i, dirs[i], errs[i])
		}
	}
	// Losers cleaned up their temp copies.
	left, _ := os.ReadDir(filepath.Dir(dirs[0]))
	if len(left) != 1 {
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("trees dir holds %v, want only the digest", names)
	}
}

func TestPlaceTreeRefusesAWrongBundle(t *testing.T) {
	_, c, _ := newComputer(t)
	tree, _ := testTree(t, stackdir.File{Path: "x", Content: []byte("1")})
	other, _ := testTree(t, stackdir.File{Path: "x", Content: []byte("2")})
	tree.Open = other.Open // the bytes do not hash to the digest
	_, err := c.(workspace.TreeHolder).PlaceTree(context.Background(), tree)
	var we *workspace.Error
	if !errors.As(err, &we) || we.Code != workspace.CodeStackDirUnavailable {
		t.Fatalf("err = %v, want %s", err, workspace.CodeStackDirUnavailable)
	}
	for _, bad := range []string{"", "zz", strings.Repeat("A", 64)} {
		if _, err := c.(workspace.TreeHolder).PlaceTree(context.Background(), workspace.Tree{Digest: bad}); err == nil {
			t.Errorf("digest %q accepted", bad)
		}
	}
}

func TestNoWorkspaceOnTheTreesDir(t *testing.T) {
	p, err := New(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []workspace.Spec{
		{Tenant: treesDir, Stack: "s", Name: "w"},
		{Tenant: "acme", Stack: ".hidden", Name: "w"},
	} {
		if _, err := p.Create(context.Background(), spec); err == nil {
			t.Errorf("created a workspace for %+v", spec)
		}
	}
}

// The Manager places the tree, starts the command beneath it and names it in
// TXCO_STACK_DIR, over whatever WITH env said.
func TestManagerExecInTree(t *testing.T) {
	p, err := New(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	unlockTrees(t, p)
	m := workspace.NewManager(p, workspace.Limits{}, nil)
	tree, _ := testTree(t,
		stackdir.File{Path: "2100_SETUP/race.py", Content: []byte("go\n")},
		stackdir.File{Path: "bin/run", Exec: true, Content: []byte("#!/bin/sh\necho ran in \"$(pwd -P)\"\n")},
	)
	spec := workspace.Spec{Tenant: "acme", Stack: "game", Name: "browser"}
	res, _, _, err := m.Exec(context.Background(), spec, workspace.ExecRequest{
		Command: `cat race.py; echo "$TXCO_STACK_DIR"; "$TXCO_STACK_DIR/bin/run"; touch x 2>/dev/null || echo read-only`,
		Cwd:     "2100_SETUP",
		Env:     map[string]string{stackdir.Env: "/not/this"},
		Tree:    &tree,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.EvalSymlinks(filepath.Join(p.Root(), treesDir, tree.Digest))
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	want := []string{"go", filepath.Join(p.Root(), treesDir, tree.Digest), "ran in " + filepath.Join(dir, "2100_SETUP"), "read-only"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("stdout:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
