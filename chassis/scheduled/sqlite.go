package scheduled

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // sqlite3 driver

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// sqliteBusyTimeoutMs is the per-connection lock-wait budget. Mirrors the
// chassis runtime/vector convention (WAL + busy_timeout, no cache=shared).
const sqliteBusyTimeoutMs = 15000

func init() {
	// Register the bundled "sqlite" backend (the default --scheduled-store):
	// a SQLite file.
	Register("sqlite", func(cfg Config) (*Store, error) {
		dbPath := cfg.DBPath
		if dbPath == "" {
			dbPath = "./chassis/data/scheduled.db"
		}
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("scheduled: create dir %s: %w", dir, err)
			}
		}
		dsn := fmt.Sprintf("file:%s?mode=rwc&_journal_mode=WAL&_busy_timeout=%d", dbPath, sqliteBusyTimeoutMs)
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			return nil, fmt.Errorf("scheduled: open %s: %w", dbPath, err)
		}
		s := NewStore(db, registry.SQLite)
		if err := s.EnsureSchema(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	})
}
