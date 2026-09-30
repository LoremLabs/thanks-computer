package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

func TestSandboxDeclSource(t *testing.T) {
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
		`INSERT INTO stacks VALUES ('stk','tnt1','support',7)`,
		`INSERT INTO stacks VALUES ('stk2','tnt2','support',9)`,
		`INSERT INTO stack_files VALUES (7,'SANDBOXES/github.yaml','env:' || char(10) || '  GH_TOKEN: secret:GITHUB_PAT' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'SANDBOXES/broken.yaml','env: []' || char(10),'')`,
		`INSERT INTO stack_files VALUES (6,'SANDBOXES/old.yaml','env:' || char(10) || '  A: secret:OLD' || char(10),'')`,
		`INSERT INTO stack_files VALUES (9,'SANDBOXES/github.yaml','env:' || char(10) || '  A: secret:X' || char(10),'')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// A fleet row: fingerprint only, bytes in the CAS.
	fleetBody := []byte("description: the warehouse\nenv:\n  PGDSN: secret:WH_DSN\n")
	sum := sha256.Sum256(fleetBody)
	fleetHash := hex.EncodeToString(sum[:])
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), fleetHash, fleetBody); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'SANDBOXES/warehouse.yaml','',?)`, fleetHash); err != nil {
		t.Fatal(err)
	}

	src := &sandboxDeclSource{dbc: &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()}, fcas: fs}
	ctx := context.Background()

	d, err := src.Lookup(ctx, "acme", "support", "github")
	if err != nil || d.Env["GH_TOKEN"] != "secret:GITHUB_PAT" {
		t.Fatalf("inline: %+v %v", d, err)
	}
	if d2, err := src.Lookup(ctx, "acme", "support", "github"); err != nil || d2 != d {
		t.Fatalf("second lookup must hit the cache: %p %p %v", d, d2, err)
	}
	w, err := src.Lookup(ctx, "acme", "support", "warehouse")
	if err != nil || w.Env["PGDSN"] != "secret:WH_DSN" || w.Description != "the warehouse" {
		t.Fatalf("fleet row via CAS: %+v %v", w, err)
	}
	// A declaration the gate would have refused is an error, not undeclared.
	if _, err := src.Lookup(ctx, "acme", "support", "broken"); err == nil || errors.Is(err, sandbox.ErrNotDeclared) {
		t.Fatalf("broken declaration: %v", err)
	}

	for _, c := range [][3]string{
		{"acme", "support", "old"},       // superseded version
		{"acme", "support", "nope"},      // never declared
		{"acme", "billing", "github"},    // other stack
		{"other", "support", "github"},   // other tenant
		{"gone", "support", "github"},    // revoked tenant
		{"acme", "support", "../github"}, // not a name
	} {
		if _, err := src.Lookup(ctx, c[0], c[1], c[2]); !errors.Is(err, sandbox.ErrNotDeclared) {
			t.Errorf("%v: want ErrNotDeclared, got %v", c, err)
		}
	}

	if names, err := src.Names(ctx, "acme", "support"); err != nil || strings.Join(names, " ") != "broken github warehouse" {
		t.Errorf("Names = %v %v", names, err)
	}
	if names, err := src.Names(ctx, "acme", "billing"); err != nil || len(names) != 0 {
		t.Errorf("Names of a stack with none = %v %v", names, err)
	}

	noCAS := &sandboxDeclSource{dbc: src.dbc}
	if _, err := noCAS.Lookup(ctx, "acme", "support", "warehouse"); err == nil || errors.Is(err, sandbox.ErrNotDeclared) {
		t.Fatalf("fingerprint row without a CAS must be an error, not undeclared: %v", err)
	}
}
