// Package blob is the name layer of the runtime blob store behind the
// txco://blob/* ops: validation of blob names and filenames, the derived-name
// recipe for user uploads, the permission vocabulary attached to names, and
// the Index — the mutable name → sha256 pointer table — over the tenant KV
// store. Bytes themselves live in the content-addressed filecas (immutable,
// global by hash); this package never touches them.
//
//	Names identify mutable concepts. Hashes identify immutable facts.
//
// Three planes, two of which are here: the CAS holds immutable values, the
// Index holds mutable application pointers (+ required permissions), and an
// application's provenance store (onepony's projections) hangs meaning off
// unnamed hashes. See docs/advanced/blobs.md.
package blob

import (
	"fmt"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxNameBytes bounds a full blob name. The KV-backed Index stores a name
	// as one key segment ("n:" + name) under the store's 256-byte segment cap,
	// so 250 leaves room; a derived name (<under>/<64 hex>) is ~80 bytes.
	MaxNameBytes = 250
	// MaxSegmentBytes bounds one '/'-separated segment of a name.
	MaxSegmentBytes = 128
	// MaxFilenameBytes bounds the verbatim `filename` metadata (UTF-8 bytes).
	MaxFilenameBytes = 255

	// IndexNamespace is the reserved KV namespace holding name rows
	// ("n:<name>"); ShaNamespace holds the tenant's sha ownership rows
	// ("s:<sha256>"). Both carry kv.ReservedNamespacePrefix, so author-facing
	// KV writers refuse them.
	IndexNamespace = "_txc.blob"
	ShaNamespace   = "_txc.blob.sha"
)

// segRe is the per-segment charset: URL-safe by construction, so a name
// needs no encoding anywhere it travels. ':' is deliberately outside it —
// the KV index substitutes ':' for '/' in keys, which stays reversible only
// because a name can never contain one.
var segRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidName reports why name is not a well-formed blob name (nil = valid):
// '/'-separated segments matching [A-Za-z0-9._-]{1,128}; no empty, "." or
// ".." segments (so no leading/trailing '/'); no leading-'_' segment
// (reserved); at most MaxNameBytes in total.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("blob name is empty")
	}
	if len(name) > MaxNameBytes {
		return fmt.Errorf("blob name exceeds %d bytes (%d)", MaxNameBytes, len(name))
	}
	for _, seg := range strings.Split(name, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("blob name %q has an empty segment (no leading, trailing or doubled '/')", name)
		case seg == "." || seg == "..":
			return fmt.Errorf("blob name %q: '.' and '..' segments are not allowed", name)
		case strings.HasPrefix(seg, "_"):
			return fmt.Errorf("blob name %q: a segment starting with '_' is reserved", name)
		case !segRe.MatchString(seg):
			return fmt.Errorf("blob name %q: segment %q must match [A-Za-z0-9._-]{1,%d}", name, seg, MaxSegmentBytes)
		}
	}
	return nil
}

// ValidFilename checks the verbatim `filename` metadata: non-empty, valid
// UTF-8, at most MaxFilenameBytes, no control characters. Anything else goes
// — the derived-name recipe (DerivedName) is what makes any real filename
// safe to address.
func ValidFilename(s string) error {
	if s == "" {
		return fmt.Errorf("filename is empty")
	}
	if len(s) > MaxFilenameBytes {
		return fmt.Errorf("filename exceeds %d bytes (%d)", MaxFilenameBytes, len(s))
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("filename is not valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("filename contains a control character")
		}
	}
	return nil
}

// ValidSha256 reports whether s is a 64-char lowercase hex sha256 — the only
// hash form the CAS and the Index accept.
func ValidSha256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// DefaultContentType guesses a content type from the extension of a name or
// filename, falling back to application/octet-stream. Used when a put or a
// seeded pack declares none.
func DefaultContentType(nameOrFilename string) string {
	if ct := mime.TypeByExtension(path.Ext(nameOrFilename)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
