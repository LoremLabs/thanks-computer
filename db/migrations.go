// Package db exposes the SQLite schema migrations as an embedded
// filesystem so the chassis binary can boot anywhere without needing
// the repo checkout on disk. The on-disk files under schema/sqlite/
// are still the source of truth — they're embedded at compile time.
package db

import "embed"

// FS contains the per-DB migration sets, embedded into the binary at
// build time. main.go reads from this when no explicit --db-schema-dir
// is provided (or when the provided dir doesn't exist).
//
// auth/ holds the identity tables (actors, keys, memberships,
// invitations, browser_*). runtime/ holds the content tables (ops,
// tenants, stacks/versions/files). Each subdirectory is swept against
// its own *sql.DB with its own changeset row in varvals.
//
// schema/postgres/auth mirrors schema/sqlite/auth for when the auth DSN
// is a postgres:// URL (the in-tree default stays SQLite — the runtime DB
// is always SQLite). See chassis/auth/registry dialect seam.
//
//go:embed schema/sqlite/auth schema/sqlite/runtime schema/postgres/auth
var FS embed.FS
