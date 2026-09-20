package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newIPPDeps builds the op deps over a temp printer store, an identity store
// and a mirror that knows the tenant `acme` (id t_acme).
func newIPPDeps(t *testing.T) ippDeps {
	t.Helper()
	dir := t.TempDir()
	store, err := chipp.Open("sqlite", chipp.Config{DBPath: filepath.Join(dir, "ipp.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mirror, err := sql.Open("sqlite3", filepath.Join(dir, "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`INSERT INTO tenants VALUES ('t_acme', 'acme', NULL)`,
	} {
		if _, err := mirror.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return ippDeps{store: store, ids: authntest.NewSQLiteStore(t), snap: func() *sql.DB { return mirror }}
}

func callIPPPrinter(t *testing.T, d ippDeps, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := ippPrinter(ctx, d, []byte(`{"_txc":{"op":"web/3337/register"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

// bindPony gives `pony:<name>` its first row, written by stack.
func bindPony(t *testing.T, ids *authn.Store, stack, name string) {
	t.Helper()
	p, err := authn.ParsePrincipal("pony:" + name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ids.BindPrincipal(context.Background(), "t_acme", stack, p,
		authn.NewBinding{Kind: authn.BindEmail, Subject: name + "@pony.example.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestIPPPrinterOp(t *testing.T) {
	d := newIPPDeps(t)
	ctx := context.Background()
	bindPony(t, d.ids, "web", "paris")
	bindPony(t, d.ids, "web", "lyon")
	bindPony(t, d.ids, "core", "theirs")
	code := func(out string) string { return gjson.Get(out, "_printer.error.code").String() }

	// Register, then register again: idempotent, and `created` says which.
	out := callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","principal":"pony:paris","display_name":"  Paris  "}`)
	if code(out) != "" || !gjson.Get(out, "_printer.created").Bool() ||
		gjson.Get(out, "_printer.printer").String() != "paris" || gjson.Get(out, "_printer.principal").String() != "pony:paris" ||
		gjson.Get(out, "_printer.display_name").String() != "Paris" || gjson.Get(out, "_printer.status").String() != "active" {
		t.Fatalf("register = %s", out)
	}
	out = callIPPPrinter(t, d, "acme", "web/canary", `{"printer":"paris","principal":"pony:paris"}`)
	if code(out) != "" || gjson.Get(out, "_printer.created").Bool() || gjson.Get(out, "_printer.display_name").String() != "Paris" {
		t.Fatalf("re-register from the canary slot = %s", out)
	}
	// The row is keyed by the tenant SLUG, which is what the head resolves.
	if p, err := d.store.GetPrinter(ctx, "acme", "paris"); err != nil || p.PrincipalID != "pony:paris" || p.CreatedBy != "web" {
		t.Fatalf("stored row: %+v %v", p, err)
	}

	// `into` moves the result; a long display name is clipped to the DNS-SD
	// instance-name limit.
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"lyon","principal":"pony:lyon","into":"_p","display_name":"`+strings.Repeat("x", 100)+`"}`)
	if got := gjson.Get(out, "_p.display_name").String(); len(got) != chipp.MaxDisplayName {
		t.Fatalf("display_name is %d bytes: %s", len(got), out)
	}

	for name, c := range map[string]struct{ tenant, stack, meta, want string }{
		"no tenant":                  {"", "web", `{"printer":"x","principal":"pony:paris"}`, "no_tenant"},
		"unknown tenant":             {"ghost", "web", `{"printer":"x","principal":"pony:paris"}`, "no_tenant"},
		"no stack":                   {"acme", "", `{"printer":"x","principal":"pony:paris"}`, "no_stack"},
		"missing printer":            {"acme", "web", `{"principal":"pony:paris"}`, "invalid_arg"},
		"a label no URL reaches":     {"acme", "web", `{"printer":"Front Desk","principal":"pony:paris"}`, "invalid_arg"},
		"missing principal":          {"acme", "web", `{"printer":"x"}`, "invalid_arg"},
		"malformed principal":        {"acme", "web", `{"printer":"x","principal":"paris"}`, "invalid_arg"},
		"bad status":                 {"acme", "web", `{"printer":"x","principal":"pony:paris","status":"paused"}`, "invalid_arg"},
		"a principal nobody wrote":   {"acme", "web", `{"printer":"x","principal":"pony:nobody"}`, "not_found"},
		"a user that does not exist": {"acme", "web", `{"printer":"x","principal":"user:usr_22222222"}`, "not_found"},
		"another stack's principal":  {"acme", "web", `{"printer":"x","principal":"pony:theirs"}`, "not_owner"},
		// Changing a printer needs the principal it has NOW, too.
		"another stack re-points it": {"acme", "core", `{"printer":"paris","principal":"pony:theirs"}`, "not_owner"},
		"another stack deletes it":   {"acme", "core", `{"printer":"paris","delete":true}`, "not_owner"},
	} {
		if got := code(callIPPPrinter(t, d, c.tenant, c.stack, c.meta)); got != "txco_ipp_"+c.want {
			t.Errorf("%s → %q, want txco_ipp_%s", name, got, c.want)
		}
	}
	if p, _ := d.store.GetPrinter(ctx, "acme", "paris"); p.PrincipalID != "pony:paris" {
		t.Fatalf("a refused call changed the printer: %+v", p)
	}
	if _, err := d.store.GetPrinter(ctx, "acme", "x"); err == nil {
		t.Fatal("a refused call registered a printer")
	}

	// Disable, re-point (the stack owns both principals), and delete.
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","principal":"pony:paris","status":"disabled"}`)
	if code(out) != "" || gjson.Get(out, "_printer.status").String() != "disabled" {
		t.Fatalf("disable = %s", out)
	}
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","principal":"pony:lyon","status":"active"}`)
	if code(out) != "" || gjson.Get(out, "_printer.principal").String() != "pony:lyon" {
		t.Fatalf("re-point = %s", out)
	}
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","delete":true}`)
	if code(out) != "" || !gjson.Get(out, "_printer.deleted").Bool() {
		t.Fatalf("delete = %s", out)
	}
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","delete":true}`)
	if code(out) != "" || gjson.Get(out, "_printer.deleted").Bool() {
		t.Fatalf("second delete = %s", out)
	}
}

// A node that does not run the ipp personality has no printer store. The op
// is registered there all the same, and says so.
func TestIPPPrinterOpWithoutAStore(t *testing.T) {
	d := newIPPDeps(t)
	d.store = nil
	out := callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","principal":"pony:paris"}`)
	if got := gjson.Get(out, "_printer.error.code").String(); got != "txco_ipp_disabled" {
		t.Fatalf("no store → %s", out)
	}
	d = newIPPDeps(t)
	d.ids = nil
	out = callIPPPrinter(t, d, "acme", "web", `{"printer":"paris","principal":"pony:paris"}`)
	if got := gjson.Get(out, "_printer.error.code").String(); got != "txco_ipp_disabled" {
		t.Fatalf("no identity store → %s", out)
	}
}
