package dataset

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// buildFixture writes a small dataset artifact (books table + FTS5 index)
// with the PLAIN driver and returns its path. The restricted driver could
// never build it — that asymmetry is itself part of what these tests pin.
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "books.sqlite")
	db, err := sql.Open("sqlite3", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE books (isbn13 TEXT PRIMARY KEY, title TEXT, author TEXT, work_id TEXT)`,
		`CREATE VIRTUAL TABLE books_fts USING fts5(title, author, content=books)`,
		`INSERT INTO books VALUES ('9780000000001', 'Angels with Dirty Faces', 'Jonathan Wilson', 'W1')`,
		`INSERT INTO books VALUES ('9780000000002', 'A Farewell to Arms', 'Ernest Hemingway', 'W2')`,
		`INSERT INTO books_fts (rowid, title, author) SELECT rowid, title, author FROM books`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	return p
}

func TestManifestParse(t *testing.T) {
	m, err := ParseManifest([]byte(`
queries:
  lookup_title:
    sql: |
      SELECT title, author FROM books_fts WHERE books_fts MATCH ? LIMIT 10
  lookup_isbn:
    sql: SELECT * FROM books WHERE isbn13 = ?
    max_rows: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Queries) != 2 {
		t.Fatalf("want 2 queries, got %d", len(m.Queries))
	}
	if m.Queries["lookup_isbn"].MaxRows != 1 {
		t.Fatalf("max_rows lost: %+v", m.Queries["lookup_isbn"])
	}
	if got := m.QueryNames(); got[0] != "lookup_isbn" || got[1] != "lookup_title" {
		t.Fatalf("QueryNames not sorted: %v", got)
	}
}

func TestManifestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top key":   "querys:\n  a:\n    sql: SELECT 1\n",
		"unknown query key": "queries:\n  a:\n    sql: SELECT 1\n    max_bytes: 3\n",
		"no queries":        "queries: {}\n",
		"empty sql":         "queries:\n  a:\n    sql: \"\"\n",
		"bad name":          "queries:\n  Bad Name:\n    sql: SELECT 1\n",
		"negative max_rows": "queries:\n  a:\n    sql: SELECT 1\n    max_rows: -1\n",
	}
	for label, body := range cases {
		if _, err := ParseManifest([]byte(body)); err == nil {
			t.Errorf("%s: expected error, got none", label)
		}
	}
}

// TestAuthorizerDenialMatrix pins the security contract: through the
// restricted driver, every write/DDL/escape shape fails and reads succeed.
func TestAuthorizerDenialMatrix(t *testing.T) {
	p := buildFixture(t)
	db, err := sql.Open(DriverName, DSN(p))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	denied := []string{
		`INSERT INTO books VALUES ('x', 'x', 'x', 'x')`,
		`UPDATE books SET title = 'x'`,
		`DELETE FROM books`,
		`DROP TABLE books`,
		`CREATE TABLE evil (x)`,
		`CREATE VIRTUAL TABLE evil USING fts5(x)`,
		`ALTER TABLE books ADD COLUMN evil TEXT`,
		`ATTACH DATABASE '/tmp/evil.sqlite' AS evil`,
		`PRAGMA journal_mode = DELETE`,
		`PRAGMA query_only = 0`,
		`VACUUM`,
		`REINDEX`,
		`ANALYZE`,
		`BEGIN`,
	}
	for _, stmt := range denied {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%q: expected denial, got success", stmt)
		} else if !strings.Contains(err.Error(), "not authorized") &&
			!strings.Contains(err.Error(), "readonly") &&
			!strings.Contains(err.Error(), "read-only") {
			// immutable/ro DSN may reject before the authorizer; either
			// layer refusing is a pass, anything else is suspicious.
			t.Logf("%q: denied by other layer: %v", stmt, err)
		}
	}

	var title string
	if err := db.QueryRow(`SELECT title FROM books WHERE isbn13 = ?`, "9780000000002").Scan(&title); err != nil {
		t.Fatalf("plain select failed: %v", err)
	}
	if title != "A Farewell to Arms" {
		t.Fatalf("wrong row: %q", title)
	}
	if err := db.QueryRow(`SELECT title FROM books_fts WHERE books_fts MATCH ?`, "angels").Scan(&title); err != nil {
		t.Fatalf("fts5 match failed: %v", err)
	}
	if title != "Angels with Dirty Faces" {
		t.Fatalf("wrong fts row: %q", title)
	}
	// Recursive CTE — the sqliteRecursive allowance.
	var n int
	if err := db.QueryRow(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 5) SELECT count(*) FROM c`).Scan(&n); err != nil {
		t.Fatalf("recursive CTE failed: %v", err)
	}
	if n != 5 {
		t.Fatalf("recursive CTE wrong result: %d", n)
	}
}

func TestIntegrityCheck(t *testing.T) {
	p := buildFixture(t)
	if err := IntegrityCheck(p); err != nil {
		t.Fatalf("good artifact: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "not-a-db.sqlite")
	if err := os.WriteFile(bad, []byte("definitely not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := IntegrityCheck(bad); err == nil {
		t.Fatal("non-sqlite file passed integrity check")
	}
}

func TestValidateArtifact(t *testing.T) {
	p := buildFixture(t)
	ctx := context.Background()

	good, _ := ParseManifest([]byte("queries:\n  by_isbn:\n    sql: SELECT title FROM books WHERE isbn13 = ?\n"))
	if err := ValidateArtifact(ctx, p, good); err != nil {
		t.Fatalf("good manifest: %v", err)
	}

	missingTable, _ := ParseManifest([]byte("queries:\n  nope:\n    sql: SELECT x FROM missing_table\n"))
	if err := ValidateArtifact(ctx, p, missingTable); err == nil {
		t.Fatal("query against missing table validated")
	}

	write, _ := ParseManifest([]byte("queries:\n  evil:\n    sql: DELETE FROM books\n"))
	if err := ValidateArtifact(ctx, p, write); err == nil {
		t.Fatal("write query validated — authorizer not engaged at prepare")
	}
}
