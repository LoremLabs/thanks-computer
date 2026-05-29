package snapshot

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

// makeSourceDB writes a minimal but shape-valid runtime DB: the four tables
// sanityCheck requires + the runtime migration changeset row + a data row.
func makeSourceDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+p+"?mode=rwc")
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer db.Close()
	_, err = db.Exec(`
		CREATE TABLE varvals (var TEXT, val TEXT, UNIQUE(var));
		CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT);
		CREATE TABLE stacks (stack_id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INT, name TEXT, txcl TEXT);
		INSERT INTO varvals (var,val) VALUES ('txco-db-changeset-runtime','7');
		INSERT INTO varvals (var,val) VALUES ('txco-control-version','1234');
		INSERT INTO tenants VALUES ('tnt_a','a');
		INSERT INTO ops VALUES ('tnt_a','web',100,'hello','EXEC "op://X"');
	`)
	if err != nil {
		t.Fatalf("seed src: %v", err)
	}
	return p
}

func TestExportBootstrapRoundTrip(t *testing.T) {
	src := makeSourceDB(t)
	data, m, err := Export(src)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if m.Kind != KindBootstrapSQLiteDump || m.Format != FormatSQLite3DumpSQL {
		t.Fatalf("manifest kind/format wrong: %+v", m)
	}
	if m.ControlVersion != 1234 {
		t.Errorf("control_version not carried: got %d", m.ControlVersion)
	}
	if m.DBMigrationVersion != "7" {
		t.Errorf("db_migration_version not carried: got %q", m.DBMigrationVersion)
	}
	if !strings.HasPrefix(m.Checksum, "sha256:") {
		t.Errorf("checksum not sha256: %q", m.Checksum)
	}

	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "runtime.db")
	if err := Bootstrap(data, m, dst, nil, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Restored DB has the data.
	db, err := sql.Open("sqlite3", "file:"+dst+"?mode=ro")
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer db.Close()
	var slug string
	if err := db.QueryRow(`SELECT slug FROM tenants WHERE tenant_id='tnt_a'`).Scan(&slug); err != nil {
		t.Fatalf("query restored: %v", err)
	}
	if slug != "a" {
		t.Errorf("restored data wrong: slug=%q", slug)
	}

	// No temp restore files left behind.
	ents, _ := os.ReadDir(dstDir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".snap-restore-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestTamperRejectedAndTargetUntouched(t *testing.T) {
	src := makeSourceDB(t)
	data, m, err := Export(src)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	tampered := append([]byte(nil), data...)
	tampered = append(tampered, []byte("\n-- evil\n")...)

	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "runtime.db")
	if err := Bootstrap(tampered, m, dst, nil, false); !errors.Is(err, ErrVerify) {
		t.Fatalf("expected ErrVerify, got %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("runtime DB must not exist after a rejected bootstrap")
	}
}

func TestIncompatibleModelRejected(t *testing.T) {
	src := makeSourceDB(t)
	data, m, _ := Export(src)
	m.ControlModelVersion = controlevent.ControlModelVersion + 1
	// Re-checksum so only the version is wrong, proving the version gate
	// (not the checksum) does the rejecting.
	m.Checksum = checksumOf(data)
	if err := Verify(data, m); !errors.Is(err, ErrVerify) {
		t.Fatalf("expected ErrVerify on model bump, got %v", err)
	}
}

func TestNoSilentDowngrade(t *testing.T) {
	src := makeSourceDB(t)
	data, m, _ := Export(src)

	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "runtime.db")
	if err := os.WriteFile(dst, []byte("existing populated db"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	if err := Bootstrap(data, m, dst, nil, false); !errors.Is(err, ErrNotFresh) {
		t.Fatalf("expected ErrNotFresh without force, got %v", err)
	}
	// Original content untouched.
	b, _ := os.ReadFile(dst)
	if string(b) != "existing populated db" {
		t.Errorf("populated DB was modified without force")
	}
	// With force it proceeds.
	if err := Bootstrap(data, m, dst, nil, true); err != nil {
		t.Fatalf("force bootstrap: %v", err)
	}
}

func TestMigrateHookInvoked(t *testing.T) {
	src := makeSourceDB(t)
	data, m, _ := Export(src)
	dst := filepath.Join(t.TempDir(), "runtime.db")
	called := false
	mig := func(dbPath string) error { called = true; return nil }
	if err := Bootstrap(data, m, dst, mig, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !called {
		t.Errorf("migrate hook was not invoked against the temp DB")
	}
}
