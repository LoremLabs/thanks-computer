package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
)

func TestOutletDeclSource(t *testing.T) {
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
		`INSERT INTO stack_files VALUES (7,'OUTLETS/crm.yaml','driver: postgres' || char(10) || 'secret: CRM_DSN' || char(10) || 'access: write' || char(10),'')`,
		`INSERT INTO stack_files VALUES (6,'OUTLETS/old.yaml','driver: postgres' || char(10) || 'secret: OLD' || char(10),'')`,
		`INSERT INTO stack_files VALUES (9,'OUTLETS/crm.yaml','driver: postgres' || char(10) || 'secret: X' || char(10),'')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// A fleet row: fingerprint only, bytes in the CAS.
	fleetBody := []byte("driver: postgres\nsecret: WH_DSN\nmax_rows: 5\n")
	sum := sha256.Sum256(fleetBody)
	fleetHash := hex.EncodeToString(sum[:])
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), fleetHash, fleetBody); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'OUTLETS/wh.yaml','',?)`, fleetHash); err != nil {
		t.Fatal(err)
	}

	src := &outletDeclSource{dbc: &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()}, fcas: fs}
	ctx := context.Background()

	d, hash, err := src.Lookup(ctx, "acme", "support", "crm")
	if err != nil || d.Secret != "CRM_DSN" || !d.Writable() || hash == "" {
		t.Fatalf("inline: %+v %q %v", d, hash, err)
	}
	d2, hash2, err := src.Lookup(ctx, "acme", "support", "crm")
	if err != nil || d2 != d || hash2 != hash {
		t.Fatalf("second lookup must hit the cache: %p %p %v", d, d2, err)
	}

	w, whash, err := src.Lookup(ctx, "acme", "support", "wh")
	if err != nil || w.Secret != "WH_DSN" || w.MaxRows != 5 || whash != fleetHash {
		t.Fatalf("fleet row via CAS: %+v %q %v", w, whash, err)
	}

	for _, c := range [][3]string{
		{"acme", "support", "old"},    // superseded version
		{"acme", "support", "nope"},   // never declared
		{"acme", "billing", "crm"},    // other stack
		{"other", "support", "crm"},   // other tenant
		{"gone", "support", "crm"},    // revoked tenant
		{"acme", "support", "../crm"}, // not a name
	} {
		if _, _, err := src.Lookup(ctx, c[0], c[1], c[2]); !errors.Is(err, outlet.ErrNotDeclared) {
			t.Errorf("%v: want ErrNotDeclared, got %v", c, err)
		}
	}

	noCAS := &outletDeclSource{dbc: src.dbc}
	if _, _, err := noCAS.Lookup(ctx, "acme", "support", "wh"); err == nil || errors.Is(err, outlet.ErrNotDeclared) {
		t.Fatalf("fingerprint row without a CAS must be an error, not undeclared: %v", err)
	}
}
