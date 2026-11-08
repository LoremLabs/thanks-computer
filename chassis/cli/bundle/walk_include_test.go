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

// `$[path.json]` is expanded by the same pass: the op carries the file's
// JSON as a literal, and Includes names the JSON files too, so `txco dev`
// watches them.
func TestWalkExpandsFileLiterals(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/core/_mail/0110_RUN/run.txcl",
		"WHEN .noun in $[../_lib/nouns.json]\n  EXEC \"txco://noop\" WITH schema = $[schema.json]")
	writeFile(t, root, "OPS/core/_mail/_lib/nouns.json", "[\"seed\", \"run\"]\n")
	writeFile(t, root, "OPS/core/_mail/0110_RUN/schema.json", "{\n  \"type\": \"object\",\n  \"required\": [\"reply\"]\n}\n")

	ops, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	want := "WHEN .noun in [\"seed\",\"run\"]\n  EXEC \"txco://noop\" WITH schema = {\"type\":\"object\",\"required\":[\"reply\"]}"
	if ops[0].Txcl != want {
		t.Errorf("txcl = %s\nwant   %s", ops[0].Txcl, want)
	}
	if len(ops[0].Includes) != 2 || ops[0].Includes[0] != "OPS/core/_mail/_lib/nouns.json" || ops[0].Includes[1] != "OPS/core/_mail/0110_RUN/schema.json" {
		t.Errorf("Includes = %v", ops[0].Includes)
	}
}

// A bad file literal stops the walk like a bad include does, naming the
// op's line and the JSON's line and column.
func TestWalkFileLiteralErrorsAreFatal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "OPS/core/0100_RUN/run.txcl", "EXEC \"txco://noop\"\n  WITH nouns = $[nouns.json]")
	writeFile(t, root, "OPS/core/0100_RUN/nouns.json", "[\"seed\",\n \"run\",]")
	_, err := Walk(root)
	want := `OPS/core/0100_RUN/run.txcl:2: $[nouns.json]: invalid JSON at line 2, column`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
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
