package db

import "embed"

// SystemOpstacksFS is the default, chassis-shipped system opstack
// bundle, embedded at build time so a fresh chassis routes correctly
// with zero files on disk. Layout mirrors the CLI bundle convention:
//
//	opstacks/<_slug>/OPS/<stack>/<scope>/<name>.txcl
//
// Only `_`-prefixed slugs are system (chassis-local, trusted) tenants.
// `_sys` owns the ingress-fallback `boot` stack. An operator can
// override/extend this from a local directory (see chassis/sysops);
// the embedded copy is the open-core baseline, exactly as
// schema/sqlite is the embedded baseline overridable by
// --db-schema-dir.
//
// `all:` is required because go:embed otherwise skips `_`-prefixed
// entries — and every system tenant dir is literally `_`-prefixed
// (`_sys`, future `_playground`, …).
//
//go:embed all:opstacks
var SystemOpstacksFS embed.FS
