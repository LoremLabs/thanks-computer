// Package storeseed implements the declarative store-seed channel: reserved
// stack subtrees (VECTORS/, KV/) that `txco apply` reconciles into the runtime
// stores (the vector store, the KV store), so a stack's known data set travels
// WITH the deploy and the tree is the desired state (sync = re-apply).
//
// This file (paths.go) is the leaf layer: just the reserved-path vocabulary,
// with no dependency on the stores. It is imported by the CLI (to collect the
// packs into the bundle), the admin producer (to CAS-back the pack bytes like
// FILES/), and the control-event applier (to keep pack bytes out of the
// in-memory runtime DB). The materializer registry that actually reconciles
// packs into stores lives alongside in this package (storeseed.go).
package storeseed

import "strings"

// Reserved top-level pack directories (siblings to OPS/ and FILES/). Each holds
// NDJSON packs: VECTORS/<collection>.jsonl, KV/<namespace>.jsonl.
const (
	DirVectors = "VECTORS"
	DirKV      = "KV"

	// Pack store kinds — the Materializer.Kind() each pack dir routes to.
	KindVector = "vector"
	KindKV     = "kv"

	// PackExt is the required extension for a pack file. We pin it so the
	// pack channel stays unambiguous (a stray FILES-style asset under
	// VECTORS/ is a deploy error, not silently reconciled).
	PackExt = ".jsonl"
)

// packDirs maps a reserved pack directory prefix ("VECTORS/") to its store kind.
var packDirs = map[string]string{
	DirVectors + "/": KindVector,
	DirKV + "/":      KindKV,
}

// IsPackPath reports whether a stack_files path is a store-seed pack — i.e. it
// lives under one of the reserved pack directories. Used by the producer /
// applier to give packs the same CAS-backed, out-of-runtime-DB treatment as
// FILES/ static assets (large pre-computed vectors must never inline into a
// control event or a data-plane node's in-memory DB).
func IsPackPath(p string) bool { return KindForPath(p) != "" }

// KindForPath returns the store kind ("vector" | "kv") a pack path routes to,
// or "" when the path is not under a reserved pack directory.
func KindForPath(p string) string {
	for prefix, kind := range packDirs {
		if strings.HasPrefix(p, prefix) {
			return kind
		}
	}
	return ""
}

// PackName returns the collection/namespace name a pack path seeds:
// "VECTORS/books.jsonl" → "books". It returns "" when the path is not a pack,
// is nested below the pack dir (no slashes allowed in the name), or lacks the
// .jsonl extension — the same shape validateStackFilePath enforces at upload.
func PackName(p string) string {
	if KindForPath(p) == "" {
		return ""
	}
	rest := p[strings.Index(p, "/")+1:] // after "VECTORS/" / "KV/"
	if rest == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, PackExt) {
		return ""
	}
	return strings.TrimSuffix(rest, PackExt)
}
