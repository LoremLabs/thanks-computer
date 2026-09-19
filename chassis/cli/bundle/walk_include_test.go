package bundle

import (
	"strings"
	"testing"
)

// `&include("…")` is expanded as ops are read: the op carries the file's
// text as a literal, and Includes names what it read.
func TestWalkExpandsIncludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/core/_mail/0110_RUN/run.txcl",
		`EXEC "workspace://w/exec" WITH args = ["sh", "-c", &include("../_lib/run.sh")]`)
	writeFile(t, root, "OPS/core/_mail/_lib/run.sh", "echo \"hi\"\n")

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	want := `EXEC "workspace://w/exec" WITH args = ["sh", "-c", "echo \"hi\"\n"]`
	if ops[0].Txcl != want {
		t.Errorf("txcl = %s\nwant   %s", ops[0].Txcl, want)
	}
	if len(ops[0].Includes) != 1 || ops[0].Includes[0] != "OPS/core/_mail/_lib/run.sh" {
		t.Errorf("Includes = %v", ops[0].Includes)
	}
}

// A bad include stops the walk with file:line; skipping the op instead
// would deploy its stack without it.
func TestWalkIncludeErrorsAreFatal(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"missing.sh", `OPS/core/0100_RUN/run.txcl:2: &include("missing.sh"): no such file`},
		// A nested stack is its own boundary: core/_mail can't reach core's files.
		{"../../x.sh", "outside the stack directory OPS/core/_mail/"},
	} {
		root := t.TempDir()
		dir := "OPS/core/0100_RUN/"
		if strings.HasPrefix(tc.rel, "../") {
			dir = "OPS/core/_mail/0100_RUN/" // ../../x.sh is OPS/core/x.sh
		}
		writeFile(t, root, "OPS/core/x.sh", "x")
		writeFile(t, root, dir+"run.txcl", "EXEC \"txco://noop\"\n  WITH a = &include(\""+tc.rel+"\")")
		_, err := Walk(root)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.rel, err, tc.want)
		}
	}
}
