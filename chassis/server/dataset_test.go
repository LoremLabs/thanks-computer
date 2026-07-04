package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// dsTestEnv wires the full op dependency set: a runtime-DB mirror holding
// the active version's DATASETS/ rows (fingerprint artifact + inline
// manifest — the single-node shape), a file CAS holding the artifact, and
// the dataset cache in front of it.
type dsTestEnv struct {
	dbc *dbcache.DbCache
	dsc *dataset.Cache
	fs  *filestore.FileStore
}

func newDsTestEnv(t *testing.T, manifest string) *dsTestEnv {
	t.Helper()

	// Build the artifact: books table + FTS5 index.
	p := filepath.Join(t.TempDir(), "books.sqlite")
	fdb, err := sql.Open("sqlite3", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE books (isbn13 TEXT PRIMARY KEY, title TEXT, author TEXT)`,
		`CREATE VIRTUAL TABLE books_fts USING fts5(title, author, content=books)`,
		`INSERT INTO books VALUES ('9780000000001','Angels with Dirty Faces','Jonathan Wilson')`,
		`INSERT INTO books VALUES ('9780000000002','A Farewell to Arms','Ernest Hemingway')`,
		`INSERT INTO books_fts (rowid, title, author) SELECT rowid, title, author FROM books`,
	} {
		if _, err := fdb.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	_ = fdb.Close()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(context.Background(), hash, data); err != nil {
		t.Fatal(err)
	}
	dsc, err := dataset.NewCache(t.TempDir(), fs, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dsc.Close)

	// Runtime mirror with just the tables resolveDataset touches.
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
		`INSERT INTO stacks VALUES ('stk','tnt1','www',7)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'DATASETS/books.sqlite','',?)`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files VALUES (7,'DATASETS/books.yaml',?,'')`, manifest); err != nil {
		t.Fatal(err)
	}

	return &dsTestEnv{
		dbc: &dbcache.DbCache{Db: db, Source: db, Logger: zap.NewNop()},
		dsc: dsc,
		fs:  fs,
	}
}

func (e *dsTestEnv) query(meta string) string {
	ctx := operation.WithMeta(processor.WithTenant(context.Background(), "acme"), meta)
	in := []byte(`{"_txc":{"route":{"stack":"www"}}}`)
	pl, _ := datasetQuery(ctx, e.dbc, e.dsc, e.fs, in, 200)
	return pl.Raw
}

const dsTestManifest = `queries:
  by_isbn:
    sql: SELECT title, author FROM books WHERE isbn13 = ?
  search:
    sql: SELECT title FROM books_fts WHERE books_fts MATCH ?
  all_books:
    sql: SELECT title FROM books ORDER BY isbn13
    max_rows: 1
`

func TestDatasetOpEndToEnd(t *testing.T) {
	env := newDsTestEnv(t, dsTestManifest)

	// Plain lookup with a bound arg.
	raw := env.query(`{"dataset":"books","query":"by_isbn","args":["9780000000002"]}`)
	if got := gjson.Get(raw, "_dataset.rows.0.title").String(); got != "A Farewell to Arms" {
		t.Fatalf("by_isbn: %s", raw)
	}
	if gjson.Get(raw, "_dataset.count").Int() != 1 || gjson.Get(raw, "_dataset.truncated").Bool() {
		t.Fatalf("by_isbn count/truncated: %s", raw)
	}

	// FTS5 MATCH through the op (the motivating use case).
	raw = env.query(`{"dataset":"books","query":"search","args":["angels"]}`)
	if got := gjson.Get(raw, "_dataset.rows.0.title").String(); got != "Angels with Dirty Faces" {
		t.Fatalf("fts search: %s", raw)
	}

	// Manifest max_rows caps below the node default and marks truncation.
	raw = env.query(`{"dataset":"books","query":"all_books"}`)
	if gjson.Get(raw, "_dataset.count").Int() != 1 || !gjson.Get(raw, "_dataset.truncated").Bool() {
		t.Fatalf("max_rows clamp: %s", raw)
	}

	// WITH limit tightens further… on a 1-cap query it's already tight; use
	// by-all via search with limit over two rows.
	raw = env.query(`{"dataset":"books","query":"by_isbn","args":["nope"]}`)
	if gjson.Get(raw, "_dataset.count").Int() != 0 {
		t.Fatalf("no-match count: %s", raw)
	}

	// Custom into path.
	raw = env.query(`{"dataset":"books","query":"by_isbn","args":["9780000000001"],"into":"_hits"}`)
	if got := gjson.Get(raw, "_hits.rows.0.title").String(); got != "Angels with Dirty Faces" {
		t.Fatalf("into: %s", raw)
	}
}

func TestDatasetOpErrors(t *testing.T) {
	env := newDsTestEnv(t, dsTestManifest)

	cases := map[string]string{ // meta → expected error code
		`{"query":"by_isbn"}`:                              "txco_dataset_invalid_arg",
		`{"dataset":"books"}`:                              "txco_dataset_invalid_arg",
		`{"dataset":"nope","query":"q"}`:                   "txco_dataset_not_found",
		`{"dataset":"books","query":"nope"}`:               "txco_dataset_unknown_query",
		`{"dataset":"books","query":"by_isbn","args":"x"}`: "txco_dataset_invalid_arg",
		`{"dataset":"books","query":"by_isbn","args":[{"o":1}]}`: "txco_dataset_invalid_arg",
	}
	for meta, wantCode := range cases {
		raw := env.query(meta)
		if got := gjson.Get(raw, "dataset.error.code").String(); got != wantCode {
			t.Errorf("meta %s: code %q want %q (%s)", meta, got, wantCode, raw)
		}
	}

	// Unknown-query error names the available queries.
	raw := env.query(`{"dataset":"books","query":"nope"}`)
	if msg := gjson.Get(raw, "dataset.error.message").String(); !strings.Contains(msg, "by_isbn") {
		t.Fatalf("unknown-query hint: %s", raw)
	}

	// No tenant scope → refused.
	ctx := operation.WithMeta(context.Background(), `{"dataset":"books","query":"by_isbn"}`)
	pl, _ := datasetQuery(ctx, env.dbc, env.dsc, env.fs, []byte(`{}`), 200)
	if got := gjson.Get(pl.Raw, "dataset.error.code").String(); got != "txco_dataset_no_tenant" {
		t.Fatalf("no tenant: %s", pl.Raw)
	}

	// No routed stack and no explicit stack → refused.
	ctx = operation.WithMeta(processor.WithTenant(context.Background(), "acme"), `{"dataset":"books","query":"by_isbn"}`)
	pl, _ = datasetQuery(ctx, env.dbc, env.dsc, env.fs, []byte(`{}`), 200)
	if got := gjson.Get(pl.Raw, "dataset.error.code").String(); got != "txco_dataset_invalid_arg" {
		t.Fatalf("no stack: %s", pl.Raw)
	}
}

