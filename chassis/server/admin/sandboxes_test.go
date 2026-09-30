package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

func TestValidateStackFilePathSandboxes(t *testing.T) {
	for _, p := range []string{"SANDBOXES/github.yaml", "SANDBOXES/deploy_ro-2.yaml"} {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{
		"SANDBOXES/github.json",   // wrong extension
		"SANDBOXES/a/github.yaml", // no nesting
		"SANDBOXES/Github.yaml",   // name rule
		"SANDBOXES/.yaml",         // empty name
		"sandboxes/github.yaml",   // exact-case channel
	} {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}

func TestDeepValidateSandboxes(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	for _, s := range []string{
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk','tnt_default','support','t')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (1,'stk',1,'draft','test','t','')`,
	} {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"SANDBOXES/github.yaml":  "description: a deploy step\nenv:\n  GH_TOKEN: secret:GITHUB_PAT\n",
		"SANDBOXES/unknown.yaml": "env:\n  A: secret:B\nnetwork: [github.com]\n",
		"SANDBOXES/bare.yaml":    "env:\n  A: GITHUB_PAT\n",
		"SANDBOXES/empty.yaml":   "description: nothing here\n",
		"100/plain.txcl":         `EXEC "txco://copy" WITH from = "a", to = "b"`,
	}
	for p, body := range files {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx,
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (1, ?, ?, '')`, p, body); err != nil {
			t.Fatal(err)
		}
	}
	issues := c.deepValidateSandboxes(ctx, 1)
	want := map[string]string{
		"SANDBOXES/unknown.yaml": "field network not found",
		"SANDBOXES/bare.yaml":    "want secret:<NAME>",
		"SANDBOXES/empty.yaml":   "env or capabilities is required",
	}
	if len(issues) != len(want) {
		t.Fatalf("want %d issues, got %+v", len(want), issues)
	}
	for _, i := range issues {
		sub, ok := want[i.Path]
		if !ok || !strings.Contains(i.Err, sub) {
			t.Errorf("unexpected issue %+v", i)
		}
	}
	// A version without sandboxes is a cheap no-op.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx, `INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (2,'stk',2,'draft','test','t','')`); err != nil {
		t.Fatal(err)
	}
	if issues := c.deepValidateSandboxes(ctx, 2); len(issues) != 0 {
		t.Fatalf("empty version: %+v", issues)
	}
}
