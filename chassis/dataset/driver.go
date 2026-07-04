package dataset

// The restricted SQLite driver every dataset connection goes through.
// Defense in depth, outermost first:
//
//  1. Only manifest-declared queries are ever executed (the op refuses
//     anything else), with parameters always bound.
//  2. The DSN opens read-only + immutable (no locks, no journal — the file
//     is content-addressed and can never change underneath us) and sets
//     query_only.
//  3. This driver's per-connection AUTHORIZER default-DENIES every action
//     class except the read set below, so even a hostile statement that
//     somehow reached prepare (a write, DDL, ATTACH, PRAGMA) fails with
//     "not authorized" — including at activation-time validation, which is
//     exactly how non-SELECT manifest queries are refused before deploy.
//
// Extension loading stays off (mattn requires an explicit opt-in we never
// give), and sqlite-vec's global auto-extension is read-only table-valued
// functions, so its presence widens nothing.

import (
	"database/sql"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// DriverName is the database/sql driver datasets open through.
const DriverName = "sqlite3_dataset"

// sqliteAuthOK is SQLITE_OK as an authorizer verdict (mattn exports
// SQLITE_DENY/SQLITE_IGNORE but not OK); sqliteRecursive is
// SQLITE_RECURSIVE — emitted for the recursive step of a WITH RECURSIVE
// CTE — which mattn has commented out. Both raw values are stable in the
// SQLite ABI.
const (
	sqliteAuthOK    = 0
	sqliteRecursive = 33
)

// readOnlyAuthorizer allows the action classes a parameterised SELECT can
// legitimately emit and denies everything else. SQLITE_READ's arg1/arg2
// (table, column) are unrestricted: the artifact holds nothing but the
// dataset itself.
func readOnlyAuthorizer(action int, arg1, arg2, arg3 string) int {
	switch action {
	case sqlite3.SQLITE_SELECT,
		sqlite3.SQLITE_READ,
		sqlite3.SQLITE_FUNCTION,
		sqliteRecursive:
		return sqliteAuthOK
	case sqlite3.SQLITE_PRAGMA:
		// Two read-only pragmas: integrity_check for IntegrityCheck, and
		// data_version because FTS5's MATCH machinery issues it internally
		// (denying it fails every full-text query). The DSN's own pragmas
		// (query_only) run before the ConnectHook installs this authorizer,
		// so they need no allowance here.
		if arg1 == "integrity_check" || arg1 == "data_version" {
			return sqliteAuthOK
		}
		return sqlite3.SQLITE_DENY
	default:
		return sqlite3.SQLITE_DENY
	}
}

func init() {
	sql.Register(DriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			// Runs AFTER the DSN's own pragmas (mode=ro, immutable,
			// query_only) have been applied, so denying SQLITE_PRAGMA here
			// cannot break the open itself.
			conn.RegisterAuthorizer(readOnlyAuthorizer)
			return nil
		},
	})
}

// DSN renders the canonical read-only immutable connection string for a
// materialised artifact file. immutable=1 promises SQLite the file cannot
// change (true: it is content-addressed), which drops all locking and
// journal handling; query_only is belt over the authorizer's braces.
func DSN(path string) string {
	return "file:" + path + "?mode=ro&immutable=1&_query_only=1"
}
