package gitstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// sqliteBusyTimeoutMs is the per-connection lock-wait budget (the chassis
// convention: WAL + busy_timeout + an immediate write lock).
const sqliteBusyTimeoutMs = 15000

func init() {
	// The bundled "sqlite" backend (the default --git-store): a SQLite
	// file of its own, never the runtime DB.
	Register("sqlite", func(cfg Config) (*Store, error) {
		dbPath := cfg.DBPath
		if dbPath == "" {
			dbPath = "./chassis/data/git.db"
		}
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("gitstore: create dir %s: %w", dir, err)
			}
		}
		dsn := fmt.Sprintf("file:%s?mode=rwc&_journal_mode=WAL&_busy_timeout=%d&_txlock=immediate", dbPath, sqliteBusyTimeoutMs)
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			return nil, fmt.Errorf("gitstore: open %s: %w", dbPath, err)
		}
		s := NewStore(db, registry.SQLite)
		if err := s.EnsureSchema(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	})
}

// OpenSQLiteFile opens the bundled backend at path (tests and tools).
func OpenSQLiteFile(path string) (*Store, error) {
	return Open("sqlite", Config{DBPath: path})
}
