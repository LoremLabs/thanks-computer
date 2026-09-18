package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/authn"
	dbschemas "github.com/loremlabs/thanks-computer/db"
)

// A node without the admin personality opens auth.db for the identity store
// (openIdentityDB). It must bring a fresh file to head, be a no-op on the
// next boot, and — unlike the admin path — hand a failure back instead of
// ending the process.
func TestOpenIdentityDB(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	dsn := "file:" + filepath.Join(t.TempDir(), "auth-test.db")

	db, dialect, err := openIdentityDB(ctx, logger, dsn, dbschemas.FS, "schema/sqlite")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if dialect != registry.SQLite {
		t.Errorf("dialect = %T", dialect)
	}
	// Migrated to head: the account plane's tables and the identity tables.
	for _, table := range []string{"actors", "oidc_subjects", "users", "principal_bindings", "credentials"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: n=%d err=%v", table, n, err)
		}
	}
	// …and usable.
	s := authn.NewStore(db, dialect)
	u, _, created, err := s.CreateUser(ctx, "tnt_a", "web", authn.NewUser{Email: "alice@example.com"})
	if err != nil || !created {
		t.Fatalf("create on the opened store: %v", err)
	}
	var changeset string
	if err := db.QueryRow(`SELECT val FROM varvals WHERE var = 'txco-db-changeset-auth'`).Scan(&changeset); err != nil || changeset != "5" {
		t.Errorf("changeset = %q err=%v", changeset, err)
	}
	_ = db.Close()

	// The next boot finds it at head and keeps the rows.
	db, _, err = openIdentityDB(ctx, logger, dsn, dbschemas.FS, "schema/sqlite")
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if got, err := authn.NewStore(db, registry.SQLite).GetUser(ctx, "tnt_a", u.ID); err != nil || got.ID != u.ID {
		t.Errorf("row lost across a reopen: %+v err=%v", got, err)
	}
	_ = db.Close()

	// A database that cannot be opened is an error, not a dead chassis.
	bad := filepath.Join(t.TempDir(), "not-a-db")
	if err := os.WriteFile(bad, []byte("this is not a sqlite file, and is long enough to be read as a header"), 0o600); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel() // skip the retry back-off
	if db, _, err := openIdentityDB(cctx, logger, "file:"+bad, dbschemas.FS, "schema/sqlite"); err == nil {
		_ = db.Close()
		t.Error("a corrupt auth.db opened without error")
	}
}
