package bundle

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeFile is a tiny helper that ensures a file's parent dir exists before
// writing. Keeps the test bodies focused on the layout under test.
func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func TestWalkEmpty(t *testing.T) {
	root := t.TempDir()
	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0 (no OPS/ dir)", len(ops))
	}
}

func TestWalkBasicPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/website/100/resonator.txcl", `EXEC "http://web/100"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	got := ops[0]
	if got.Stack != "website" || got.Scope != 100 || got.Name != "resonator" {
		t.Errorf("got (stack=%s, scope=%d, name=%s), want (website, 100, resonator)",
			got.Stack, got.Scope, got.Name)
	}
	if got.Txcl != `EXEC "http://web/100"` {
		t.Errorf("got txcl %q, want bytes preserved verbatim", got.Txcl)
	}
}

// TestWalkMultipleRulesPerScope is the central feature: any number of
// `*.txcl` files in a scope dir become parallel rules at the same stage.
func TestWalkMultipleRulesPerScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/website/100/hello.txcl", `EXEC "http://hello"`)
	writeFile(t, root, "OPS/website/100/world.txcl", `EXEC "http://world"`)
	writeFile(t, root, "OPS/website/100/audit.txcl", `EXEC "http://audit"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("got %d ops, want 3: %+v", len(ops), ops)
	}

	got := make(map[string]string)
	for _, op := range ops {
		if op.Stack != "website" || op.Scope != 100 {
			t.Errorf("op %+v: unexpected (stack, scope)", op)
		}
		got[op.Name] = op.Txcl
	}
	want := map[string]string{
		"hello": `EXEC "http://hello"`,
		"world": `EXEC "http://world"`,
		"audit": `EXEC "http://audit"`,
	}
	for name, txcl := range want {
		if got[name] != txcl {
			t.Errorf("name=%s: got txcl %q, want %q", name, got[name], txcl)
		}
	}
}

// TestWalkScopeDirNamingForms covers the `NNNN`, `NNNN_DESCRIPTION`, and
// leading-zero variants that the OLD repo used and that the user's layout
// example expects (`0000_SETUP`, `0100_TRIAGE`).
func TestWalkScopeDirNamingForms(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/department/0000_SETUP/init.txcl", `EXEC "http://setup"`)
	writeFile(t, root, "OPS/department/0100_TRIAGE/triage.txcl", `EXEC "http://triage"`)
	writeFile(t, root, "OPS/department/0500/cleanup.txcl", `EXEC "http://cleanup"`)
	writeFile(t, root, "OPS/department/700/report.txcl", `EXEC "http://report"`) // no padding

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 4 {
		t.Fatalf("got %d ops, want 4: %+v", len(ops), ops)
	}

	type k struct {
		stack string
		scope int
		name  string
	}
	got := make(map[k]bool)
	for _, op := range ops {
		got[k{op.Stack, op.Scope, op.Name}] = true
	}
	want := []k{
		{"department", 0, "init"},
		{"department", 100, "triage"},
		{"department", 500, "cleanup"},
		{"department", 700, "report"},
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing rule %+v in %+v", w, got)
		}
	}
}

func TestWalkSlashedStackName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/website/canary/100/resonator.txcl", `EXEC "http://canary/100"`)
	writeFile(t, root, "OPS/website/canary/eu/200/resonator.txcl", `EXEC "http://canary-eu/200"`)
	writeFile(t, root, "OPS/website/100/resonator.txcl", `EXEC "http://web/100"`)
	writeFile(t, root, "OPS/department/canary/0100_TRIAGE/triage.txcl", `EXEC "http://dept-canary"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	type k struct {
		stack string
		scope int
		name  string
	}
	got := make(map[k]string)
	for _, op := range ops {
		got[k{op.Stack, op.Scope, op.Name}] = op.Txcl
	}

	want := map[k]string{
		{"website", 100, "resonator"}:                `EXEC "http://web/100"`,
		{"website/canary", 100, "resonator"}:         `EXEC "http://canary/100"`,
		{"website/canary/eu", 200, "resonator"}:      `EXEC "http://canary-eu/200"`,
		{"department/canary", 100, "triage"}:         `EXEC "http://dept-canary"`,
	}
	if len(got) != len(want) {
		t.Errorf("got %d records, want %d", len(got), len(want))
	}
	for key, txcl := range want {
		if got[key] != txcl {
			t.Errorf("key %+v: got %q, want %q", key, got[key], txcl)
		}
	}
}

func TestWalkMockSiblings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/website/100/resonator.txcl", `EXEC "http://web/100"`)
	writeFile(t, root, "OPS/website/100/mock-request.json", `{"x":1}`)
	writeFile(t, root, "OPS/website/100/mock-response.json", `{"ok":true}`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if ops[0].MockReq != `{"x":1}` {
		t.Errorf("MockReq = %q, want canonicalized JSON", ops[0].MockReq)
	}
	if ops[0].MockRes != `{"ok":true}` {
		t.Errorf("MockRes = %q, want canonicalized JSON", ops[0].MockRes)
	}
}

func TestWalkSkipsNonTxcl(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/website/100/resonator.txcl", `EXEC "http://web/100"`)
	writeFile(t, root, "OPS/website/100/README.md", "documentation")
	writeFile(t, root, "OPS/website/notes.txt", "scratch")
	// File matching the *.txcl glob but not under <stack>/<scope>/. Skipped.
	writeFile(t, root, "OPS/website/loose.txcl", `EXEC "http://stray"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 1 {
		t.Errorf("got %d ops, want 1 (only the well-formed one): %+v", len(ops), ops)
	}
}

func TestWalkSourcePathSet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/a/100/resonator.txcl", `EXEC "http://a/100"`)
	writeFile(t, root, "OPS/b/200/resonator.txcl", `EXEC "http://b/200"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Stack < ops[j].Stack })

	for _, op := range ops {
		if op.SourcePath == "" {
			t.Errorf("op %s/%d/%s: SourcePath is empty", op.Stack, op.Scope, op.Name)
		}
	}
}
