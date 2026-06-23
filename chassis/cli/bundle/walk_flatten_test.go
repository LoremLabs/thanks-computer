package bundle

import (
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
)

// dirFS mirrors what Walk does internally so the diag-returning entry points
// can be exercised against a temp dir.
func dirFS(root string) fs.FS { return os.DirFS(root) }

type sk struct {
	stack string
	scope int
	name  string
}

func opSet(ops []Op) map[sk]bool {
	got := make(map[sk]bool, len(ops))
	for _, op := range ops {
		got[sk{op.Stack, op.Scope, op.Name}] = true
	}
	return got
}

// TestWalkFlattenOrgDirBelowStep: a `.txcl` nested under an organization dir
// below the numbered step inherits that step, and its name is the path from
// the step dir flattened to the op-name charset.
func TestWalkFlattenOrgDirBelowStep(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/www/_mail/1000_setup/config.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/www/_mail/1000_setup/misc/normalize.txcl", `EXEC "txco://noop"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	got := opSet(ops)
	want := []sk{
		{"www/_mail", 1000, "config"},
		{"www/_mail", 1000, "misc_normalize"},
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %+v\nactual: %+v", w, got)
		}
	}
}

// TestWalkFlattenNearestNumberedWins: with two numbered dirs on the path, the
// deepest (nearest the leaf) sets the step; the shallowest still delimits the
// stack root. No stride arithmetic.
func TestWalkFlattenNearestNumberedWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/www/_mail/1000_setup/1010_config/load.txcl", `EXEC "txco://noop"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	got := ops[0]
	if got.Stack != "www/_mail" || got.Scope != 1010 || got.Name != "load" {
		t.Errorf("got (%s, %d, %s), want (www/_mail, 1010, load)", got.Stack, got.Scope, got.Name)
	}
}

// TestWalkFlattenCollision: two leaves that flatten to the same
// (stack, scope, name) produce a collision diagnostic naming both paths, and
// neither (well, only the first) lands as an op.
func TestWalkFlattenCollision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/app/1000_setup/misc/config.txcl", `EXEC "txco://a"`)
	writeFile(t, root, "OPS/app/1000_setup/misc_config.txcl", `EXEC "txco://b"`)

	_, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diags, want 1: %+v", len(diags), diags)
	}
	d := diags[0]
	if d.Stack != "app" {
		t.Errorf("collision diag stack = %q, want app", d.Stack)
	}
	for _, want := range []string{"misc/config.txcl", "misc_config.txcl", "app/1000/misc_config"} {
		if !strings.Contains(d.Msg, want) {
			t.Errorf("collision msg missing %q:\n%s", want, d.Msg)
		}
	}
}

// TestWalkFlattenNoNumberedAncestor: a `.txcl` with no numbered dir on its path
// is a no-step diagnostic (loud), not a silent drop, and yields no op.
func TestWalkFlattenNoNumberedAncestor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/www/_mail/load.txcl", `EXEC "txco://noop"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diags, want 1: %+v", len(diags), diags)
	}
	if !strings.Contains(diags[0].Msg, "no numbered step directory") {
		t.Errorf("diag msg = %q, want no-step explanation", diags[0].Msg)
	}
}

// TestWalkFlattenEmptyStack: a top-level numeric directory reads as a step,
// leaving no stack name — reported, not deployed.
func TestWalkFlattenEmptyStack(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/2024/0100/x.txcl", `EXEC "txco://noop"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Msg, "no stack name") {
		t.Fatalf("want one empty-stack diag, got: %+v", diags)
	}
}

// TestWalkFlattenDisabledOrgDir: a `_`-prefixed organization dir below the step
// disables every leaf beneath it (park-a-draft affordance) — silently, no diag.
func TestWalkFlattenDisabledOrgDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/app/1000_setup/live.txcl", `EXEC "txco://live"`)
	writeFile(t, root, "OPS/app/1000_setup/_disabled/draft.txcl", `EXEC "txco://draft"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("disabled dir should not diagnose: %+v", diags)
	}
	got := opSet(ops)
	if !got[sk{"app", 1000, "live"}] {
		t.Errorf("missing live op: %+v", got)
	}
	if got[sk{"app", 1000, "draft"}] {
		t.Errorf("disabled draft.txcl should not deploy: %+v", got)
	}
}

// TestWalkFlattenLeadingSystemSegmentNotDisabled: a `_`-prefixed segment ABOVE
// the numbered step is a stack segment (`_mail`, `_sys`), not a disable marker.
func TestWalkFlattenLeadingSystemSegmentNotDisabled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/www/_mail/1000_setup/load.txcl", `EXEC "txco://noop"`)

	ops, _, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if !opSet(ops)[sk{"www/_mail", 1000, "load"}] {
		t.Errorf("www/_mail leaf should deploy, got: %+v", ops)
	}
}

// TestWalkSystemFlattenNesting: system (`_`-prefixed root) stacks get the same
// flatten treatment via WalkSystemFS, and app strays don't error the sys walk.
func TestWalkSystemFlattenNesting(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/_sys/boot/100/route/handler.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/app/loose.txcl", `EXEC "txco://stray"`) // app no-step; sys walk ignores

	ops, err := WalkSystemFS(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkSystemFS: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d sys ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Stack != "_sys/boot" || ops[0].Scope != 100 || ops[0].Name != "route_handler" {
		t.Errorf("got (%s, %d, %s), want (_sys/boot, 100, route_handler)",
			ops[0].Stack, ops[0].Scope, ops[0].Name)
	}
}

// TestWalkSeparatorFormsParseStep locks the numbered-dir separator forms the
// doc allows: `_`, `-`, and whitespace after the digits.
func TestWalkSeparatorFormsParseStep(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/a/1000_setup/x.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/b/1000-setup/x.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/c/1000 setup/x.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/d/1000/x.txcl", `EXEC "txco://noop"`)

	ops, diags, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diags: %+v", diags)
	}
	got := opSet(ops)
	for _, stack := range []string{"a", "b", "c", "d"} {
		if !got[sk{stack, 1000, "x"}] {
			t.Errorf("stack %s did not parse to step 1000: %+v", stack, got)
		}
	}
}

// TestWalkDiagDeterministic: diagnostics and ops come back in a stable order so
// apply's output doesn't flap (WalkDir visits lexically).
func TestWalkDiagDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/z/9000/a.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/a/1000/b.txcl", `EXEC "txco://noop"`)

	ops, _, err := WalkFSDiag(dirFS(root), ".")
	if err != nil {
		t.Fatalf("WalkFSDiag: %v", err)
	}
	if !sort.SliceIsSorted(ops, func(i, j int) bool { return ops[i].SourcePath < ops[j].SourcePath }) {
		t.Errorf("ops not in lexical source-path order: %+v", ops)
	}
}
