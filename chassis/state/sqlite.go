package state

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
// runtime/notebook convention: WAL + busy_timeout, no cache=shared).
const sqliteBusyTimeoutMs = 15000

func init() {
	// Register the bundled "sqlite" backend (the default --state-store): a
	// SQLite file of its own, never the runtime DB (the dbcache watcher
	// reloads the whole runtime mirror on any runtime-file write).
	//
	// _txlock=immediate is load-bearing: a transition is a two-statement
	// transaction (the CAS UPDATE, then the event INSERT) and this DSN
	// takes the write lock at BEGIN, so two transitions never interleave
	// and the losing one fails fast instead of deadlocking mid-way.
	Register("sqlite", func(cfg Config) (*Store, error) {
		dbPath := cfg.DBPath
		if dbPath == "" {
			dbPath = "./chassis/data/state.db"
		}
		if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("state: create dir %s: %w", dir, err)
			}
		}
		dsn := fmt.Sprintf("file:%s?mode=rwc&_journal_mode=WAL&_busy_timeout=%d&_txlock=immediate", dbPath, sqliteBusyTimeoutMs)
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			return nil, fmt.Errorf("state: open %s: %w", dbPath, err)
		}
		s := NewStore(db, registry.SQLite)
		if err := s.EnsureSchema(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	})
}
