// Package snapshot turns the runtime SQLite DB into a content-addressed,
// checksummed, model-versioned artifact and restores one SAFELY.
//
// Safety invariants (see internal docs/todo-architecture-saas-fleet.md and the plan):
//   - Restore is replace, never in-place execute. The dump SQL is never run
//     against the live/migrated runtime DB. It is restored into a fresh temp
//     DB, migrated + sanity-checked there, then atomically renamed into place.
//   - No silent downgrade. Bootstrap applies only when the runtime DB is
//     fresh; replacing a populated DB requires an explicit force.
//   - Incompatible artifacts are rejected loudly (checksum + model/cache
//     version + kind/format), never silently misrouted.
//
// It reuses the existing sqlite3dump primitive (chassis/dbcache uses the
// same library) — this is codification of an existing capability, not new
// machinery.
package snapshot

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/schollz/sqlite3dump"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

const (
	// KindBootstrapSQLiteDump is the only artifact kind this package
	// produces/consumes today. A different kind is rejected by Verify.
	KindBootstrapSQLiteDump = "bootstrap.sqlite.dump"
	// FormatSQLite3DumpSQL is the on-disk payload format (SQL text).
	FormatSQLite3DumpSQL = "sqlite3dump.sql"

	// runtimeChangesetVar mirrors cmd/txco/main.go's runtime migration
	// changeset key in varvals.
	runtimeChangesetVar = "txco-db-changeset-runtime"
	// controlVersionVar carries the fleet cursor across dump/restore so a
	// restored node knows its resume position.
	controlVersionVar = "txco-control-version"
)

// Manifest is the artifact sidecar. Every field is written now so a future
// incompatibility is rejected cleanly without a format change.
type Manifest struct {
	Kind                 string `json:"kind"`
	Format               string `json:"format"`
	Checksum             string `json:"checksum"` // "sha256:<hex>"
	ControlModelVersion  int    `json:"control_model_version"`
	CacheSchemaVersion   int    `json:"cache_schema_version"`
	ControlVersion       uint64 `json:"control_version"`
	CreatedAt            string `json:"created_at"`
	SourceChassisVersion string `json:"source_chassis_version"`
	DBMigrationVersion   string `json:"db_migration_version"`
}

var (
	// ErrVerify is returned when an artifact fails integrity/compatibility.
	ErrVerify = errors.New("snapshot: artifact verification failed")
	// ErrNotFresh is returned when Bootstrap would replace a populated DB
	// without force (the no-silent-downgrade invariant).
	ErrNotFresh = errors.New("snapshot: runtime DB is not fresh (use force to replace)")
)

func sourceChassisVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

func checksumOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// readVarval reads a single varvals row; "" if absent. Caller's sql driver
// (go-sqlite3) must be registered.
func readVarval(dbPath, key string) (string, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var val string
	err = db.QueryRow(`SELECT val FROM varvals WHERE var = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return val, nil
}

// Export dumps runtimeDBPath into a self-contained artifact + manifest.
func Export(runtimeDBPath string) ([]byte, Manifest, error) {
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	if err := sqlite3dump.Dump(runtimeDBPath, w); err != nil {
		return nil, Manifest{}, fmt.Errorf("snapshot: dump %s: %w", runtimeDBPath, err)
	}
	if err := w.Flush(); err != nil {
		return nil, Manifest{}, err
	}
	data := b.Bytes()

	mig, err := readVarval(runtimeDBPath, runtimeChangesetVar)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("snapshot: read migration version: %w", err)
	}
	var cv uint64
	if s, err := readVarval(runtimeDBPath, controlVersionVar); err == nil && s != "" {
		_, _ = fmt.Sscan(s, &cv)
	}

	m := Manifest{
		Kind:                 KindBootstrapSQLiteDump,
		Format:               FormatSQLite3DumpSQL,
		Checksum:             checksumOf(data),
		ControlModelVersion:  controlevent.ControlModelVersion,
		CacheSchemaVersion:   controlevent.CacheSchemaVersion,
		ControlVersion:       cv,
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		SourceChassisVersion: sourceChassisVersion(),
		DBMigrationVersion:   mig,
	}
	return data, m, nil
}

// Verify checks integrity and compatibility. Loud failure, never silent.
func Verify(data []byte, m Manifest) error {
	if m.Kind != KindBootstrapSQLiteDump {
		return fmt.Errorf("%w: unexpected kind %q", ErrVerify, m.Kind)
	}
	if m.Format != FormatSQLite3DumpSQL {
		return fmt.Errorf("%w: unexpected format %q", ErrVerify, m.Format)
	}
	if got := checksumOf(data); got != m.Checksum {
		return fmt.Errorf("%w: checksum mismatch (manifest=%s actual=%s)",
			ErrVerify, m.Checksum, got)
	}
	if !controlevent.CompatibleModel(m.ControlModelVersion) {
		return fmt.Errorf("%w: incompatible control_model_version %d (this binary=%d)",
			ErrVerify, m.ControlModelVersion, controlevent.ControlModelVersion)
	}
	if !controlevent.CompatibleCacheSchema(m.CacheSchemaVersion) {
		return fmt.Errorf("%w: incompatible cache_schema_version %d (this binary=%d)",
			ErrVerify, m.CacheSchemaVersion, controlevent.CacheSchemaVersion)
	}
	return nil
}

// RestoreToTempDB verifies data, then writes it into a BRAND-NEW sqlite file
// in destDir. It never touches the runtime DB. The returned path is the temp
// DB; the caller is responsible for migrating, sanity-checking and renaming
// (or removing) it. On any error nothing durable is left behind.
func RestoreToTempDB(data []byte, m Manifest, destDir string) (string, error) {
	if err := Verify(data, m); err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(destDir, ".snap-restore-*.db")
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	_ = f.Close()
	_ = os.Remove(tmpPath) // sqlite creates it; avoid an empty pre-file

	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	db, err := sql.Open("sqlite3", "file:"+tmpPath+"?mode=rwc")
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := db.Exec(string(data)); err != nil {
		_ = db.Close()
		cleanup()
		return "", fmt.Errorf("snapshot: restore dump: %w", err)
	}
	if err := db.Close(); err != nil {
		cleanup()
		return "", err
	}
	return tmpPath, nil
}

// IsFresh reports whether the runtime DB at path is empty/absent. A non-fresh
// DB must not be replaced without force (no-silent-downgrade).
func IsFresh(runtimeDBPath string) bool {
	fi, err := os.Stat(runtimeDBPath)
	if err != nil {
		return true // absent → fresh
	}
	return fi.Size() == 0
}

// sanityCheck opens the (migrated) temp DB and asserts the minimum shape a
// chassis needs before we make it live.
func sanityCheck(dbPath string) error {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('ops','stacks','tenants','varvals')`,
	).Scan(&n); err != nil {
		return fmt.Errorf("%w: sanity query: %v", ErrVerify, err)
	}
	if n < 4 {
		return fmt.Errorf("%w: restored DB missing required tables (found %d/4)", ErrVerify, n)
	}
	var changeset string
	err = db.QueryRow(`SELECT val FROM varvals WHERE var = ?`, runtimeChangesetVar).Scan(&changeset)
	if err != nil || changeset == "" {
		return fmt.Errorf("%w: restored DB has no runtime migration changeset", ErrVerify)
	}
	return nil
}

// Bootstrap is the safe end-to-end restore: verify → restore into a fresh
// temp DB → migrate that temp DB → sanity-check → atomically rename into
// place. The runtime DB is untouched until the final rename. migrate runs
// the chassis migrator against the given (temp) DB path.
//
// If the runtime DB is not fresh, Bootstrap refuses unless force is true.
func Bootstrap(data []byte, m Manifest, runtimeDBPath string,
	migrate func(dbPath string) error, force bool) error {

	if err := Verify(data, m); err != nil {
		return err
	}
	if !force && !IsFresh(runtimeDBPath) {
		return ErrNotFresh
	}

	destDir := filepath.Dir(runtimeDBPath)
	tmpPath, err := RestoreToTempDB(data, m, destDir)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if migrate != nil {
		if err := migrate(tmpPath); err != nil {
			return fmt.Errorf("snapshot: migrate restored DB: %w", err)
		}
	}
	if err := sanityCheck(tmpPath); err != nil {
		return err
	}

	// Same-directory rename → atomic replace on POSIX.
	if err := os.Rename(tmpPath, runtimeDBPath); err != nil {
		return fmt.Errorf("snapshot: atomic install: %w", err)
	}
	renamed = true
	return nil
}
