package storeseed

import "strings"

// Reserved store-seed trees under OPS/<stack>/. Each maps to a pack kind
// owned by one Materializer (vecseed, kvseed, blobseed).
const (
	DirVectors = "VECTORS"
	DirKV      = "KV"
	DirBlobs   = "BLOBS"

	KindVector = "vector"
	KindKV     = "kv"
	KindBlob   = "blob"

	PackExt = ".jsonl"
)

var packDirs = map[string]string{
	DirVectors + "/": KindVector,
	DirKV + "/":      KindKV,
	DirBlobs + "/":   KindBlob,
}

// IsPackPath reports whether p lives in a store-seed tree.
func IsPackPath(p string) bool { return KindForPath(p) != "" }

// KindForPath returns the pack kind for p ("" when p is not a pack path).
// Prefix match, exact-case — the SQL classification downstream is LIKE
// 'VECTORS/%' etc., and Postgres LIKE is case-sensitive.
func KindForPath(p string) string {
	for prefix, kind := range packDirs {
		if strings.HasPrefix(p, prefix) {
			return kind
		}
	}
	return ""
}

// IsBlobPath reports whether p is a BLOBS/ pack row (one blob per file).
func IsBlobPath(p string) bool { return KindForPath(p) == KindBlob }

// PackName returns the name the pack owns, or "" when p is not a well-formed
// pack path. VECTORS/ and KV/ packs are a single "<name>.jsonl" segment (the
// collection / namespace is unambiguous; no nesting). A BLOBS/ row is the
// opposite shape: the tree IS the hierarchy, so the name is everything after
// "BLOBS/" — "BLOBS/faqs/house-01.doc" owns the blob name "faqs/house-01.doc".
// Whether that is a VALID blob name is the blob package's call
// (blob.ValidName), enforced at the write boundary and by the collector.
func PackName(p string) string {
	kind := KindForPath(p)
	if kind == "" {
		return ""
	}
	rest := p[strings.Index(p, "/")+1:] // after "VECTORS/" / "KV/" / "BLOBS/"
	if kind == KindBlob {
		return rest // "" for a bare "BLOBS/" — caller rejects
	}
	if rest == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, PackExt) {
		return ""
	}
	return strings.TrimSuffix(rest, PackExt)
}

// EmptyTree is the marker pack a Reconciler passes to a kind whose tree was
// removed entirely between versions (Path == ""), so the materializer still
// runs its delete-missing pass. Only the blob kind consumes it today; the
// single-file kinds keep "pack removed = stop managing".
func EmptyTree(kind string) RawPack { return RawPack{Kind: kind} }
