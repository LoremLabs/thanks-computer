package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
)

// capDeclFixture: tenant acme has two active stacks (loop v7, pony-web v8),
// a superseded version (6), and a revoked tenant holds a stack too.
func capDeclFixture(t *testing.T) *capDeclSource {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, s := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE stacks (stack_id TEXT PRIMARY KEY, tenant_id TEXT, name TEXT, active_version INTEGER)`,
		`CREATE TABLE stack_files (version_id INTEGER, path TEXT, content TEXT, content_hash TEXT)`,
		`INSERT INTO tenants VALUES ('tnt1','acme',NULL)`,
		`INSERT INTO tenants VALUES ('tnt2','gone','2026-01-01')`,
		`INSERT INTO stacks VALUES ('stk','tnt1','loop',7)`,
		`INSERT INTO stacks VALUES ('stk3','tnt1','pony-web',8)`,
		`INSERT INTO stacks VALUES ('stk2','tnt2','loop',9)`,
		`INSERT INTO stack_files VALUES (7,'CAPS/mail.send.yaml','description: Send a message.' || char(10) || 'entry: 7000' || char(10) || 'input:' || char(10) || '  to:' || char(10) || '    required: true' || char(10) || 'timeout: 60000' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'CAPS/broken.yaml','entry: []' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'CAPS/Not.A.Name.yaml','entry: 7000' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'SANDBOXES/workstation.yaml','capabilities: [mail.send]' || char(10),'')`,
		`INSERT INTO stack_files VALUES (6,'CAPS/old.yaml','entry: 7000' || char(10),'')`,
		`INSERT INTO stack_files VALUES (8,'CAPS/local.web.fetch.yaml','entry: 2007' || char(10),'')`,
		`INSERT INTO stack_files VALUES (9,'CAPS/secret.cap.yaml','entry: 100' || char(10),'')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// A fleet row: fingerprint only, bytes in the CAS.
	fleetBody := []byte("description: Finish the run.\nentry: 7300\n")
	sum := sha256.Sum256(fleetBody)
	fleetHash := hex.EncodeToString(sum[:])
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), fleetHash, fleetBody); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'CAPS/run.finish.yaml','',?)`, fleetHash); err != nil {
		t.Fatal(err)
	}
	return &capDeclSource{dbc: &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()}, fcas: fs}
}

func TestCapDeclSourceLookup(t *testing.T) {
	src := capDeclFixture(t)
	ctx := context.Background()

	stack, d, err := src.Lookup(ctx, "acme", "mail.send")
	if err != nil || stack != "loop" || d.Entry != 7000 || d.Stage(stack) != "loop/7000" || !d.Input["to"].Required {
		t.Fatalf("inline: %q %+v %v", stack, d, err)
	}
	if _, d2, err := src.Lookup(ctx, "acme", "mail.send"); err != nil || d2 != d {
		t.Fatalf("second lookup must hit the cache: %p %p %v", d, d2, err)
	}
	// The name is looked up across the tenant's active stacks.
	if stack, d, err := src.Lookup(ctx, "acme", "local.web.fetch"); err != nil || stack != "pony-web" || d.Entry != 2007 {
		t.Fatalf("another stack's capability: %q %+v %v", stack, d, err)
	}
	stack, f, err := src.Lookup(ctx, "acme", "run.finish")
	if err != nil || stack != "loop" || f.Entry != 7300 || f.Description != "Finish the run." {
		t.Fatalf("fleet row via CAS: %q %+v %v", stack, f, err)
	}
	// A declaration the gate would have refused is an error, not undeclared.
	if _, _, err := src.Lookup(ctx, "acme", "broken"); err == nil || errors.Is(err, capdecl.ErrNotDeclared) {
		t.Fatalf("broken declaration: %v", err)
	}

	for _, c := range [][2]string{
		{"acme", "old"},         // superseded version
		{"acme", "nope"},        // never declared
		{"acme", "secret.cap"},  // another tenant's
		{"other", "mail.send"},  // other tenant
		{"gone", "secret.cap"},  // revoked tenant
		{"acme", "../mail"},     // not a name
		{"acme", "Not.A.Name"},  // not a name, though a row exists
		{"acme", "workstation"}, // a sandbox, not a capability
	} {
		if _, _, err := src.Lookup(ctx, c[0], c[1]); !errors.Is(err, capdecl.ErrNotDeclared) {
			t.Errorf("%v: want ErrNotDeclared, got %v", c, err)
		}
	}

	noCAS := &capDeclSource{dbc: src.dbc}
	if _, _, err := noCAS.Lookup(ctx, "acme", "run.finish"); err == nil || errors.Is(err, capdecl.ErrNotDeclared) {
		t.Fatalf("fingerprint row without a CAS must be an error, not undeclared: %v", err)
	}
}

func TestCapDeclSourceList(t *testing.T) {
	src := capDeclFixture(t)
	ctx := context.Background()

	got, err := src.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name, stack string
		broken      bool
	}{
		{"broken", "loop", true},
		{"local.web.fetch", "pony-web", false},
		{"mail.send", "loop", false},
		{"run.finish", "loop", false},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %+v", len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Name != w.name || e.Stack != w.stack || (e.Err != nil) != w.broken || (e.Decl == nil) != w.broken {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
	}

	// A duplicate that slipped past the gate lists once, under the stack
	// Lookup would pick.
	if _, err := src.dbc.Db.Exec(`INSERT INTO stack_files VALUES (8,'CAPS/mail.send.yaml','entry: 2007' || char(10),'')`); err != nil {
		t.Fatal(err)
	}
	got, err = src.List(ctx, "acme")
	if err != nil || len(got) != len(want) || got[2].Name != "mail.send" || got[2].Stack != "loop" {
		t.Fatalf("duplicate: %+v %v", got, err)
	}
	if stack, _, err := src.Lookup(ctx, "acme", "mail.send"); err != nil || stack != "loop" {
		t.Fatalf("duplicate lookup: %q %v", stack, err)
	}

	for _, tenant := range []string{"other", "gone"} {
		if got, err := src.List(ctx, tenant); err != nil || len(got) != 0 {
			t.Errorf("List(%s) = %+v %v", tenant, got, err)
		}
	}
}
