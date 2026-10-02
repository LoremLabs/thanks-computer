// Package computesrc is the source of a compute op, kept beside the wasm it
// was built from so the admin can show what a `compute://sha256/<digest>`
// does. The wasm is compiled with javy's `source=omitted`, so without this
// the authored JS/TS never reaches the chassis at all.
//
// `txco apply` encodes one Bundle per compute it builds from source and
// uploads it to the hash-addressed file store (filecas). The stack version
// then carries one fingerprint-only row per compute it uses:
//
//	COMPUTES/<wasm digest>.json   content "", content_hash = sha256(bundle)
//
// so an unchanged compute costs each new version one small row and its bytes
// are stored once. The admin reads a bundle only through such a row in the
// caller's own stack — never by hash alone.
//
// This file is the leaf layer: the reserved-path vocabulary and the bundle
// codec, with no dependency on stores, so the CLI, the admin API and the
// control-event applier can import it cheaply.
package computesrc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Dir is the reserved top-level directory for compute-source rows.
const Dir = "COMPUTES"

// Version is the bundle format version.
const Version = 1

var pathRe = regexp.MustCompile(`^` + Dir + `/([0-9a-f]{64})\.json$`)

// Path is the stack_files path that records the source of the compute whose
// wasm digest (sha256, lowercase hex) is digest.
func Path(digest string) string { return Dir + "/" + digest + ".json" }

// IsPath reports whether a stack_files path lives under COMPUTES/.
func IsPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// DigestFromPath returns the wasm digest a COMPUTES/ row names, or "" when p
// is not exactly COMPUTES/<64 lowercase hex>.json.
func DigestFromPath(p string) string {
	m := pathRe.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return m[1]
}

// File is one source file of a compute. Path is relative to the entry's
// directory, slash-separated ("pull_plan.js", "../lib/util.js"), so the same
// source gives the same bundle wherever the stack sits on disk.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Bundle is a compute's source: its entry file plus every local file the
// entry imports. The embedded @txco/op SDK is not included.
type Bundle struct {
	V     int    `json:"v"`
	Entry string `json:"entry"`
	Files []File `json:"files"`
}

// Encode returns the bundle's canonical bytes and their sha256 (lowercase
// hex) — the filecas key. Files are sorted by path, so equal sources encode
// identically on any machine.
func (b Bundle) Encode() ([]byte, string, error) {
	if err := b.validate(); err != nil {
		return nil, "", err
	}
	files := append([]File(nil), b.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	data, err := json.Marshal(Bundle{V: Version, Entry: b.Entry, Files: files})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// Decode parses and checks a bundle read back from the store.
func Decode(data []byte) (Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("compute source: %w", err)
	}
	if b.V != Version {
		return Bundle{}, fmt.Errorf("compute source: unsupported version %d", b.V)
	}
	if err := b.validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func (b Bundle) validate() error {
	if len(b.Files) == 0 {
		return errors.New("compute source: no files")
	}
	seen := make(map[string]bool, len(b.Files))
	hasEntry := false
	for _, f := range b.Files {
		if !validFilePath(f.Path) {
			return fmt.Errorf("compute source: bad file path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("compute source: duplicate file %q", f.Path)
		}
		seen[f.Path] = true
		if f.Path == b.Entry {
			hasEntry = true
		}
	}
	if !hasEntry {
		return fmt.Errorf("compute source: entry %q is not among the files", b.Entry)
	}
	return nil
}

// validFilePath accepts a clean, relative, slash-separated path. Leading
// "../" segments are allowed (an import from a sibling directory); nothing
// absolute, empty or unclean.
func validFilePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	return path.Clean(p) == p && p != "." && p != ".."
}
