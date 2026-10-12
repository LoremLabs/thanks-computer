package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/calldecl"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
)

func callDeclFixture(t *testing.T) *callDeclSource {
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
		`INSERT INTO stacks VALUES ('stk','tnt1','app/_orient',7)`,
		`INSERT INTO stacks VALUES ('stk3','tnt1','app',8)`,
		`INSERT INTO stacks VALUES ('stk2','tnt2','app/_orient',9)`,
		`INSERT INTO stack_files VALUES (7,'CALLS/orient.yaml','description: Compile the context.' || char(10) || 'entry: 100' || char(10) || 'input:' || char(10) || '  job:' || char(10) || '    required: true' || char(10) || 'output:' || char(10) || '  prompt:' || char(10) || '    required: true' || char(10) || 'timeout: 20000' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'CALLS/broken.yaml','entry: []' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'CALLS/Not.Valid.yaml','entry: 100' || char(10),'')`,
		`INSERT INTO stack_files VALUES (7,'CAPS/mail.send.yaml','entry: 100' || char(10),'')`,
		`INSERT INTO stack_files VALUES (6,'CALLS/old.yaml','entry: 100' || char(10),'')`,
		`INSERT INTO stack_files VALUES (8,'CALLS/onboard.yaml','entry: 200' || char(10),'')`,
		`INSERT INTO stack_files VALUES (9,'CALLS/orient.yaml','entry: 1' || char(10),'')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	// A fleet row: fingerprint only, bytes in the CAS.
	fleetBody := []byte("description: Validate a record.\nentry: 300\n")
	sum := sha256.Sum256(fleetBody)
	fleetHash := hex.EncodeToString(sum[:])
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), fleetHash, fleetBody); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'CALLS/validate.yaml','',?)`, fleetHash); err != nil {
		t.Fatal(err)
	}
	return &callDeclSource{dbc: &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()}, fcas: fs}
}

func TestCallDeclSourceLookup(t *testing.T) {
	src := callDeclFixture(t)
	ctx := context.Background()

	d, err := src.Lookup(ctx, "acme", "app/_orient", "orient")
	if err != nil || d.Entry != 100 || d.Timeout != 20000 || !d.Input["job"].Required || !d.Output["prompt"].Required {
		t.Fatalf("orient: %+v %v", d, err)
	}
	// The fleet row resolves through the CAS.
	if d, err := src.Lookup(ctx, "acme", "app/_orient", "validate"); err != nil || d.Entry != 300 {
		t.Fatalf("fleet row: %+v %v", d, err)
	}
	// An entry is a stack's own: the same name on another stack is another
	// entry, and a stack without it declares nothing.
	if d, err := src.Lookup(ctx, "acme", "app", "onboard"); err != nil || d.Entry != 200 {
		t.Fatalf("app/onboard: %+v %v", d, err)
	}
	for _, c := range [][3]string{
		{"acme", "app", "orient"},         // not this stack's
		{"acme", "app/_orient", "old"},    // a superseded version's
		{"acme", "app/_orient", "nope"},   // never declared
		{"gone", "app/_orient", "orient"}, // a revoked tenant's
		{"acme", "app/_orient", "Not.Valid"},
		{"acme", "app/_orient", ""},
	} {
		if _, err := src.Lookup(ctx, c[0], c[1], c[2]); !errors.Is(err, calldecl.ErrNotDeclared) {
			t.Errorf("Lookup(%v) = %v, want ErrNotDeclared", c, err)
		}
	}
	// A row that does not parse is an error naming the entry, not a pass.
	if _, err := src.Lookup(ctx, "acme", "app/_orient", "broken"); err == nil || errors.Is(err, calldecl.ErrNotDeclared) {
		t.Errorf("broken: %v", err)
	}
}

func TestCallDeclSourceList(t *testing.T) {
	src := callDeclFixture(t)
	entries, err := src.List(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by stack then name; the misnamed row is skipped; the broken
	// one is listed with its error.
	var got []string
	for _, e := range entries {
		s := e.Stack + ":" + e.Name
		if e.Err != nil {
			s += "!"
		}
		got = append(got, s)
	}
	want := []string{"app:onboard", "app/_orient:broken!", "app/_orient:orient", "app/_orient:validate"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entries[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if entries, err := src.List(context.Background(), "gone"); err != nil || len(entries) != 0 {
		t.Errorf("a revoked tenant lists nothing: %v %v", entries, err)
	}
}
