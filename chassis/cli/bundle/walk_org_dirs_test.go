package bundle

import "testing"

// TestWalkOrganizationalDirs locks in support for arbitrary org-level
// directories above the scope. The user-facing convention is "walk the
// tree for *.txcl; the immediate parent of each file is the scope; the
// path above it (slashes preserved) is the stack." Anything goes for
// the path above the scope — `depts/accounting`, `customer-success`,
// `dept-0001` — as long as it doesn't itself look like a scope dir.
func TestWalkOrganizationalDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/depts/accounting/1000/mything.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/dept-0001/1/seed.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/customer-success/triage/0500_PAGER/alert.txcl", `EXEC "txco://noop"`)
	writeFile(t, root, "OPS/single-level/0/router.txcl", `EXEC "txco://noop"`)

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
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
		{"depts/accounting", 1000, "mything"},
		{"dept-0001", 1, "seed"},
		{"customer-success/triage", 500, "alert"},
		{"single-level", 0, "router"},
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing rule %+v\nactual: %+v", w, got)
		}
	}
}
