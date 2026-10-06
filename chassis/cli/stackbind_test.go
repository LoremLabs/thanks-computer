package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadStackBindings(t *testing.T) {
	root := writeConfig(t, "txco.yaml", `
target: dev
stacks:
  web:
    abi: www/txco-web
  paddock:
    abi: ../paddock/txco-web
`)
	b, err := loadStackBindings(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2 || b["web"].ABI != "www/txco-web" || b["web"].Abs != filepath.Join(root, "www", "txco-web") {
		t.Fatalf("bindings = %+v", b)
	}
	if b["paddock"].Abs != filepath.Join(filepath.Dir(root), "paddock", "txco-web") {
		t.Fatalf(".. must resolve against the root: %+v", b["paddock"])
	}

	// JSON configs carry the same block.
	root = writeConfig(t, "txco.json", `{"stacks":{"web":{"abi":"www/txco-web"}}}`)
	if b, err := loadStackBindings(root); err != nil || b["web"].ABI != "www/txco-web" {
		t.Fatalf("json: %+v %v", b, err)
	}

	// No config, or no stacks:, is no bindings.
	if b, err := loadStackBindings(t.TempDir()); err != nil || b != nil {
		t.Fatalf("no config: %+v %v", b, err)
	}
	if b, err := loadStackBindings(writeConfig(t, "txco.yaml", "target: dev\n")); err != nil || len(b) != 0 {
		t.Fatalf("no stacks: %+v %v", b, err)
	}
	// An unparseable config without a stacks: block is ignored, as it always was.
	if b, err := loadStackBindings(writeConfig(t, "txco.yaml", "target: [unclosed\n")); err != nil || b != nil {
		t.Fatalf("broken config without stacks: %+v %v", b, err)
	}
}

func TestLoadStackBindingsRefuses(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"unknown key":         {"stacks:\n  web:\n    path: www\n", `unknown key "path"`},
		"scalar value":        {"stacks:\n  web: www/txco-web\n", "must be a mapping"},
		"no abi":              {"stacks:\n  web: {}\n", "no abi"},
		"empty abi":           {"stacks:\n  web:\n    abi: \"\"\n", "must be a path"},
		"absolute":            {"stacks:\n  web:\n    abi: /tmp/x\n", "relative"},
		"home":                {"stacks:\n  web:\n    abi: ~/x\n", "relative"},
		"inside OPS":          {"stacks:\n  web:\n    abi: OPS/web/txco-web\n", "inside OPS/"},
		"the root":            {"stacks:\n  web:\n    abi: .\n", "workspace root"},
		"a channel stack":     {"stacks:\n  web/_mail:\n    abi: www\n", "channel stack"},
		"a system stack":      {"stacks:\n  _cron:\n    abi: www\n", "system or channel"},
		"boot":                {"stacks:\n  boot:\n    abi: www\n", "boot pipeline"},
		"a bad name":          {"stacks:\n  \"we b\":\n    abi: www\n", "invalid segment"},
		"same dir":            {"stacks:\n  a:\n    abi: www\n  b:\n    abi: www\n", "same or nested"},
		"nested dirs":         {"stacks:\n  a:\n    abi: www\n  b:\n    abi: www/inner\n", "same or nested"},
		"stack: typo":         {"stack:\n  web:\n    abi: www\n", "did you mean `stacks:`"},
		"broken with stacks:": {"stacks:\n  web: [unclosed\n", "can't be ignored"},
		"not a mapping":       {"stacks: [web]\n", "must map"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadStackBindings(writeConfig(t, "txco.yaml", c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
