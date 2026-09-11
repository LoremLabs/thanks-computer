package notebook

import (
	"fmt"
	"regexp"
	"strings"
)

// Name grammar — the blob grammar, adopted so hierarchical names
// (task/42, conversation/alice, workspace/session-abc) work and a prefix
// listing is meaningful. KV's own grammar forbids '/' and would reject
// every one of those.
const (
	// MaxNameBytes bounds a whole name.
	MaxNameBytes = 250
	// MaxSegmentBytes bounds one '/'-separated segment.
	MaxSegmentBytes = 128
	// MaxTypeBytes bounds an entry type.
	MaxTypeBytes = 128
	// MaxObjectKeyBytes bounds an object_key: it sits in a btree index, and
	// a Postgres index tuple caps well under this (same guard as the
	// scheduled store's idempotency key).
	MaxObjectKeyBytes = 512
	// maxSegBytes bounds a tenant or namespace segment (the KV bound).
	maxSegBytes = 256
)

var (
	segRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	typeRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// ValidName reports why name is not a well-formed notebook name (nil =
// valid): '/'-separated segments matching [A-Za-z0-9._-]{1,128}; no empty,
// "." or ".." segments (so no leading/trailing '/'); no leading-'_' segment
// (reserved); at most MaxNameBytes in total.
func ValidName(name string) error {
	if name == "" {
		return &InvalidNameError{Reason: "notebook name is empty"}
	}
	if len(name) > MaxNameBytes {
		return &InvalidNameError{Reason: fmt.Sprintf("notebook name exceeds %d bytes (%d)", MaxNameBytes, len(name))}
	}
	for _, seg := range strings.Split(name, "/") {
		switch {
		case seg == "":
			return &InvalidNameError{Reason: fmt.Sprintf("notebook name %q has an empty segment (no leading, trailing or doubled '/')", name)}
		case seg == "." || seg == "..":
			return &InvalidNameError{Reason: fmt.Sprintf("notebook name %q: '.' and '..' segments are not allowed", name)}
		case strings.HasPrefix(seg, "_"):
			return &InvalidNameError{Reason: fmt.Sprintf("notebook name %q: a segment starting with '_' is reserved", name)}
		case !segRe.MatchString(seg):
			return &InvalidNameError{Reason: fmt.Sprintf("notebook name %q: segment %q must match [A-Za-z0-9._-]{1,%d}", name, seg, MaxSegmentBytes)}
		}
	}
	return nil
}

// ValidType reports why t is not a well-formed entry type (nil = valid):
// [A-Za-z0-9._:-]{1,128} — task.created, command.finished,
// recipient.resolved.
func ValidType(t string) error {
	if t == "" {
		return &InvalidArgError{Reason: "type is empty"}
	}
	if len(t) > MaxTypeBytes {
		return &TooLargeError{Field: "type", Max: MaxTypeBytes, Got: len(t)}
	}
	if !typeRe.MatchString(t) {
		return &InvalidArgError{Reason: fmt.Sprintf("type %q must match [A-Za-z0-9._:-]{1,%d}", t, MaxTypeBytes)}
	}
	return nil
}

// segOK validates a tenant or namespace segment: non-empty, bounded, and
// free of the '/' separator and control characters so the composed
// notebook_id is unambiguous and a caller cannot escape its scope. The KV
// store's rule, verbatim.
func segOK(s string) bool {
	if s == "" || len(s) > maxSegBytes {
		return false
	}
	for _, r := range s {
		if r == '/' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
