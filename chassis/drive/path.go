package drive

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/loremlabs/thanks-computer/chassis/opname"
)

// Path limits. A path is the "/"-joined segments below the collection root;
// the root itself is the empty path.
const (
	MaxPathBytes    = 1024
	MaxPathSegments = 32
	MaxSegmentBytes = 255
)

// NormalizePath canonicalizes a resource path: NFC (so "é" typed two ways
// is one name — macOS clients send NFD), leading/trailing "/" trimmed,
// each segment non-empty, not "." or "..", valid UTF-8, no NUL and no "/"
// (by construction), within the byte and depth limits. Returns "" for the
// root ("", "/", "//"). Rejects with ErrBadPath (wrapped, with the reason).
func NormalizePath(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrBadPath)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("%w: contains NUL", ErrBadPath)
	}
	p = norm.NFC.String(p)
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	segs := strings.Split(p, "/")
	if len(segs) > MaxPathSegments {
		return "", fmt.Errorf("%w: more than %d segments", ErrBadPath, MaxPathSegments)
	}
	for _, seg := range segs {
		switch seg {
		case "":
			return "", fmt.Errorf("%w: empty segment", ErrBadPath)
		case ".", "..":
			return "", fmt.Errorf("%w: %q segment", ErrBadPath, seg)
		}
		if len(seg) > MaxSegmentBytes {
			return "", fmt.Errorf("%w: segment over %d bytes", ErrBadPath, MaxSegmentBytes)
		}
	}
	if len(p) > MaxPathBytes {
		return "", fmt.Errorf("%w: over %d bytes", ErrBadPath, MaxPathBytes)
	}
	return p, nil
}

// ParentOf returns the parent path of a normalized path ("" for a top-level
// resource or the root).
func ParentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// BaseOf returns the last segment of a normalized path ("" for the root).
func BaseOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Depth is the number of segments of a normalized path (0 for the root).
func Depth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

// JoinPath joins a normalized parent and a segment.
func JoinPath(parent, seg string) string {
	if parent == "" {
		return seg
	}
	return parent + "/" + seg
}

// IsInside reports whether p is strictly below dir (both normalized). The
// root ("") contains every non-root path.
func IsInside(p, dir string) bool {
	if p == "" {
		return false
	}
	if dir == "" {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}

// LikePrefix is the LIKE pattern matching every path strictly below p, with
// LIKE metacharacters escaped; the SQL that uses it must say
// `ESCAPE '\'` (likeEscape).
func LikePrefix(p string) string {
	return opname.EscapeLike(p) + "/%"
}

// likeEscape is the ESCAPE clause that pairs with LikePrefix.
const likeEscape = ` ESCAPE '` + opname.LikeEscapeChar + `'`
