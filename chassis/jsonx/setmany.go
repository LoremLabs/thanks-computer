package jsonx

import (
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PathVal is one Set for SetMany.
type PathVal struct {
	Path string
	Val  any
}

// SetMany applies the sets to doc with the exact semantics of the
// sequential `doc, _ = sjson.Set(doc, path, v)` chain. When every
// target path already exists (the steady-state for periodic re-stamps
// like budget sync), it resolves all spans in one pass and splices one
// output buffer instead of copying the whole doc per set; otherwise it
// falls back to the sequential chain, so byte-identity holds by
// construction.
func SetMany(doc string, sets []PathVal) string {
	if out, ok := setManyFast(doc, sets); ok {
		return out
	}
	for _, s := range sets {
		doc, _ = sjson.Set(doc, s.Path, s.Val)
	}
	return doc
}

type manySplice struct {
	start, end int
	raw        string
}

// plainSetManyPath: dotted literal segments only — no escapes, force
// prefixes, wildcards, or empty segments. Numeric segments are fine
// (they resolve as object keys or in-range array indexes; out-of-range
// would fail the Exists gate below anyway).
func plainSetManyPath(p string) bool {
	if p == "" {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '/' || c == '$' || c == ' ' || c == '.':
		default:
			return false
		}
	}
	return p[0] != '.' && p[len(p)-1] != '.' && !strings.Contains(p, "..")
}

func setManyFast(doc string, sets []PathVal) (string, bool) {
	if len(sets) == 0 {
		return doc, true
	}
	if len(doc) == 0 || doc[0] != '{' || !gjson.Valid(doc) {
		return "", false
	}
	splices := make([]manySplice, 0, len(sets))
	for _, s := range sets {
		if !plainSetManyPath(s.Path) {
			return "", false
		}
		// Resolve the replace target the way sjson does: segment by
		// segment, tracking the absolute span.
		cur := doc
		base := 0
		for _, seg := range strings.Split(s.Path, ".") {
			r := gjson.Get(cur, seg)
			if !r.Exists() || r.Index <= 0 || r.Index+len(r.Raw) > len(cur) ||
				cur[r.Index:r.Index+len(r.Raw)] != r.Raw {
				return "", false // missing → sjson would insert; not a splice
			}
			base += r.Index
			cur = r.Raw
		}
		raw, ok := encodeValue(s.Val)
		if !ok {
			return "", false
		}
		splices = append(splices, manySplice{base, base + len(cur), raw})
	}
	sort.Slice(splices, func(i, j int) bool { return splices[i].start < splices[j].start })
	for i := 1; i < len(splices); i++ {
		if splices[i].start < splices[i-1].end {
			// overlapping targets (nested or duplicate paths) are
			// order-dependent — the chain gets those right
			return "", false
		}
	}
	var b strings.Builder
	grow := len(doc)
	for _, sp := range splices {
		grow += len(sp.raw) - (sp.end - sp.start)
	}
	if grow < len(doc) {
		grow = len(doc)
	}
	b.Grow(grow)
	pos := 0
	for _, sp := range splices {
		b.WriteString(doc[pos:sp.start])
		b.WriteString(sp.raw)
		pos = sp.end
	}
	b.WriteString(doc[pos:])
	return b.String(), true
}
