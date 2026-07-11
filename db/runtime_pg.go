package db

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// The open-core binary embeds only the SQLite runtime schema — the
// Postgres runtime schema is owned by the cloud overlay (see the embed
// note in migrations.go and chassis/auth/registry dialect seam). To keep
// zero Postgres in open-core while still letting the boot migration
// runner drive a postgres:// runtime DSN, the overlay REGISTERS its
// embedded schema tree here at init() time (blank-imported by the cloud
// main). Open-core never references the overlay symbol; it only reads
// back an fs.FS + root through the getter below. If nothing is registered
// and a postgres:// runtime DSN is used, the caller fails closed — an
// open-core binary has no business opening a Postgres runtime.
var (
	runtimePGMu   sync.Mutex
	runtimePGFS   fs.FS
	runtimePGRoot string
	runtimePGSet  bool
)

// RegisterRuntimePostgresSchema records the overlay's embedded Postgres
// runtime schema (its fs.FS and the root directory inside it that holds
// the NNNN_*.sql migration files). Called from the overlay's init().
func RegisterRuntimePostgresSchema(fsys fs.FS, root string) {
	runtimePGMu.Lock()
	defer runtimePGMu.Unlock()
	runtimePGFS = fsys
	runtimePGRoot = root
	runtimePGSet = true
}

// RuntimePostgresSchema returns the registered Postgres runtime schema
// (fs.FS, root) and whether one was registered. ok is false in an
// open-core binary that never blank-imported the overlay.
func RuntimePostgresSchema() (fsys fs.FS, root string, ok bool) {
	runtimePGMu.Lock()
	defer runtimePGMu.Unlock()
	return runtimePGFS, runtimePGRoot, runtimePGSet
}

// ApplyRuntimeSQLite applies the embedded SQLite runtime schema
// (schema/sqlite/runtime/*.sql, in numeric order) directly to db. It is
// the schema half of the dbcache Postgres mirror loader: the mirror is a
// throwaway :memory: SQLite handle, so unlike the boot migration runner
// this does NO changeset/varvals bookkeeping — it just materializes the
// table shapes the hot-read path expects, into which the loader then
// copies rows from the authoritative Postgres store.
//
// Ordering matters (later migrations ALTER/rebuild earlier tables); the
// NNNN_ prefixes are zero-padded so a lexical sort is the numeric order.
func ApplyRuntimeSQLite(db *sql.DB) error {
	const root = "schema/sqlite/runtime"
	ents, err := fs.ReadDir(FS, root)
	if err != nil {
		return fmt.Errorf("read runtime schema dir: %w", err)
	}
	var files []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, rerr := fs.ReadFile(FS, path.Join(root, f))
		if rerr != nil {
			return fmt.Errorf("read runtime schema %s: %w", f, rerr)
		}
		// go-sqlite3 executes a multi-statement string in one Exec (the
		// same guarantee the boot migration runner relies on), so a whole
		// migration file applies atomically per file.
		if _, eerr := db.Exec(string(b)); eerr != nil {
			return fmt.Errorf("apply runtime schema %s: %w", f, eerr)
		}
	}
	return nil
}
