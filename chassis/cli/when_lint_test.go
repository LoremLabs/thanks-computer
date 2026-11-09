package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
)

func TestLintWhenZeroEquality(t *testing.T) {
	root := t.TempDir()
	rules := map[string]string{
		// Warned: true when the path was never written.
		"warn_false.txcl":  "WHEN ._check.ok == false\n  EMIT .error = \"no\"\n",
		"warn_zero.txcl":   "WHEN @src == \"http\" && ._count.n == 0\n  EMIT .empty = true\n",
		"warn_float.txcl":  "WHEN ._price.total == 0.0\n  EMIT .free = true\n",
		"warn_or.txcl":     "WHEN ._a.ok == false || ._b.ok == true\n  EMIT .x = 1\n",
		"warn_deeper.txcl": "WHEN exists ._other && ._check.ok == false\n  EMIT .x = 1\n",
		// Not warned.
		"ok_guarded.txcl":        "WHEN exists ._check && ._check.ok == false\n  EMIT .error = \"no\"\n",
		"ok_guarded_exact.txcl":  "WHEN ._check.ok == false && exists ._check.ok\n  EMIT .error = \"no\"\n",
		"ok_not_true.txcl":       "WHEN ._check.ok != true\n  EMIT .error = \"no\"\n",
		"ok_negated.txcl":        "WHEN !(._check.ok == false)\n  EMIT .x = 1\n",
		"ok_true.txcl":           "WHEN ._check.ok == true\n  EMIT .x = 1\n",
		"ok_empty_string.txcl":   "WHEN ._check.code == \"\"\n  EMIT .x = 1\n",
		"ok_null.txcl":           "WHEN ._check.code == null\n  EMIT .x = 1\n",
		"ok_nonzero.txcl":        "WHEN ._count.n == 3\n  EMIT .x = 1\n",
		"ok_no_when.txcl":        "EMIT .x = 1\n",
		"ok_guarded_nested.txcl": "WHEN exists ._check && (._check.ok == false || ._check.ok == true)\n  EMIT .x = 1\n",
	}
	for name, body := range rules {
		writeFile(t, filepath.Join(root, "OPS/site/100", name), body)
	}
	ops, _, err := bundle.WalkDiag(root)
	if err != nil {
		t.Fatal(err)
	}
	warnings := lintWhenZeroEquality(ops)
	got := map[string]string{}
	for _, w := range warnings {
		for name := range rules {
			if strings.Contains(w, "/"+strings.TrimSuffix(name, ".txcl")+")") {
				got[name] = w
			}
		}
	}
	for name := range rules {
		want := strings.HasPrefix(name, "warn_")
		if _, has := got[name]; has != want {
			t.Errorf("%s: warned %v, want %v (%s)", name, has, want, got[name])
		}
	}
	if len(warnings) != 5 {
		t.Errorf("want 5 warnings, got %d:\n%s", len(warnings), strings.Join(warnings, "\n"))
	}
	if w := got["warn_false.txcl"]; !strings.Contains(w, "`._check.ok == false`") || !strings.Contains(w, "missing path") {
		t.Errorf("message = %s", w)
	}
	if w := got["warn_zero.txcl"]; !strings.Contains(w, "`._count.n == 0`") {
		t.Errorf("message = %s", w)
	}

	// Through the command: a warning, not a failure.
	code, out, _ := runLintArgs(t, root)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (warnings don't fail); out=%q", code, out)
	}
	if !strings.Contains(out, "warning(s)") || !strings.Contains(out, "a missing path reads as the zero value") {
		t.Fatalf("missing the WHEN warning in: %q", out)
	}
}
