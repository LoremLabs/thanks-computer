package storeseed

import "bytes"

// RawPack is a store-seed pack as it travels through the deploy: the
// stack_files path it came from, the store kind + owned name derived from that
// path, and the resolved NDJSON bytes (one item per line). The control plane
// reads the bytes inline from stack_files; a data-plane node resolves them from
// the shared CAS by fingerprint. The materializer framework parses Bytes into
// typed items per kind.
type RawPack struct {
	Path  string // e.g. "VECTORS/books.jsonl"
	Kind  string // KindVector | KindKV
	Name  string // collection / namespace the pack owns
	Bytes []byte // raw NDJSON
}

// NewRawPack builds a RawPack from a pack path + its resolved bytes, deriving
// Kind and Name from the path. It returns (RawPack{}, false) when the path is
// not a well-formed pack path (caller should have validated, but this keeps the
// loader honest).
func NewRawPack(path string, b []byte) (RawPack, bool) {
	kind := KindForPath(path)
	name := PackName(path)
	if kind == "" || name == "" {
		return RawPack{}, false
	}
	return RawPack{Path: path, Kind: kind, Name: name, Bytes: b}, true
}

// Lines returns the pack's non-empty, whitespace-trimmed NDJSON lines, in file
// order. Blank lines (and a trailing newline) are skipped so an editor-friendly
// pack with stray blank lines still parses cleanly.
func (p RawPack) Lines() [][]byte {
	var out [][]byte
	for _, ln := range bytes.Split(p.Bytes, []byte("\n")) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		out = append(out, ln)
	}
	return out
}
