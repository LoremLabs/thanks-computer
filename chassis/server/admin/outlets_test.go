package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
)

func TestValidateStackFilePathOutlets(t *testing.T) {
	for _, p := range []string{"OUTLETS/crm.yaml", "OUTLETS/crm_ro-2.yaml", "SOURCES/mailboxes.jsonl"} {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{
		"OUTLETS/crm.json",        // wrong extension
		"OUTLETS/a/crm.yaml",      // no nesting
		"OUTLETS/Crm.yaml",        // name rule
		"OUTLETS/.yaml",           // empty name
		"outlets/crm.yaml",        // exact-case channel
		"sources/mailboxes.jsonl", // exact-case channel (SOURCES joins the list)
	} {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}

func TestDeepValidateOutlets(t *testing.T) {
	outlet.Register(&outlettest.Driver{DriverName: "fake"})
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
		"OUTLETS/crm.yaml":    "driver: fake\nsecret: CRM_DSN\naccess: read\n",
		"OUTLETS/broken.yaml": "driver: mysql\nsecret: X\n",
		"100/lookup.txcl":     `EXEC "outlet://crm/query" WITH sql = "SELECT id FROM customers WHERE email = $1", args = &array(.email)`,
		"200/write.txcl":      `EXEC "outlet://crm/exec" WITH sql = "UPDATE customers SET plan = $1"`,
		"300/nope.txcl":       `EXEC "outlet://nope/query" WITH sql = "SELECT 1"`,
		"400/two.txcl":        `EXEC "outlet://crm/query" WITH sql = "SELECT 1; DROP TABLE customers"`,
		"500/plain.txcl":      `EXEC "txco://copy" WITH from = "a", to = "b"`,
	}
	for p, body := range files {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx,
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (1, ?, ?, '')`, p, body); err != nil {
			t.Fatal(err)
		}
	}
	issues := c.deepValidateOutlets(ctx, 1)
	want := map[string]string{
		"OUTLETS/broken.yaml": "unknown driver",
		"200/write.txcl":      "access: read",
		"300/nope.txcl":       "not declared",
		"400/two.txcl":        "more than one statement",
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
	// A version without outlets or outlet ops is a cheap no-op.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx, `INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (2,'stk',2,'draft','test','t','')`); err != nil {
		t.Fatal(err)
	}
	if issues := c.deepValidateOutlets(ctx, 2); len(issues) != 0 {
		t.Fatalf("empty version: %+v", issues)
	}
}
