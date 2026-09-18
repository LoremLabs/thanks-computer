package txcguard

import (
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// sjson treats an all-digit path key as an ARRAY INDEX, and when the array is
// missing or too short it pads with `null` up to that index. So a path is a
// size: `EMIT .a.2000000 = 1` turns a 33-byte rule into a 10 MB envelope, and
// a larger index asks for gigabytes — one rule, and the node is out of memory.
//
// Everywhere an AUTHOR-supplied path reaches a JSON write (EMIT, SET, LOOP
// SET, SELECT … AS, a WITH key, `&set`, a `secrets.body.*` path, an op's
// `into`/`to`/`output_path`), the write goes through BoundedSet* below, which
// refuses one that would pad by more than MaxArrayPad elements. Paths the
// chassis writes itself are literals and don't need it.

// MaxArrayPad is the most `null` elements one author-directed write may make
// sjson add. 65,535 × `null,` is ~320 KB — far past any real sparse array,
// far short of hurting the node.
const MaxArrayPad = 65535

// ErrArrayPad is returned by BoundedSet* for a write PadsArray refuses.
var ErrArrayPad = errors.New("path would pad an array past the limit: a key in digits is an array index — write it `:123` to make it an object key (quoted in a txcl path: `.\":123\"`)")

// PadsArray reports whether writing path into doc would make sjson pad arrays
// by more than MaxArrayPad elements in total.
//
// It looks at doc, not just the path, because a numeric key is only an index
// where sjson would build or extend an array: under a missing parent, a
// scalar, or an array shorter than the index. Under an existing OBJECT the
// same key is a plain object key (`by_id.1690000000` into `{"by_id":{}}`),
// and so is a `:`-forced one anywhere — those are never refused.
func PadsArray(doc, path string) bool {
	return padsArray(doc, path, MaxArrayPad)
}

func padsArray(doc, path string, max int) bool {
	// Fast path, allocation-free: with k digits in the whole path, the
	// indexes in it sum to at most 10^k-1 (all k digits in one key). Nearly
	// every path has none.
	if !digitsCanExceed(path, max) {
		return false
	}
	segs, ok := jsonx.PathSegments(path)
	if !ok {
		return false // sjson rejects the path: nothing is written
	}

	// What follows replays sjson's own walk (appendRawPaths), level by level,
	// with the same gjson.Get it uses to decide "this part exists" — so the
	// verdict cannot drift from the writer on an odd key (an empty one, a
	// gjson modifier, a repeated member). Where sjson would emit
	// appendRepeat("null,", n) we add n to pad instead.
	for len(segs) > 0 {
		if res := gjson.Get(doc, segs[0].Raw); res.Index > 0 {
			if len(segs) == 1 {
				return false // replaces an existing value in place
			}
			doc, segs = res.Raw, segs[1:]
			continue
		}
		break
	}
	if len(segs) == 0 {
		return false
	}

	// segs[0] does not exist in doc: sjson builds everything from here.
	pad := 0
	add := func(k jsonx.PathKey) (over bool) {
		n, isIndex, huge := k.Index()
		if !isIndex {
			return false
		}
		if huge || n > max-pad {
			return true
		}
		pad += n
		return false
	}
	n, isIndex, huge := segs[0].Index()
	parent := gjson.Parse(doc)
	if isIndex && parent.IsArray() {
		// Extends an existing array: only the new elements are padding.
		if huge {
			return true
		}
		if grow := n - arrayLen(parent); grow > 0 {
			if grow > max {
				return true
			}
			pad = grow
		}
	} else if isIndex && !parent.IsObject() {
		// No document (or a scalar) here: sjson starts a fresh array.
		if add(segs[0]) {
			return true
		}
	}
	// Under an object segs[0] is a plain key. Either way every numeric key
	// AFTER it opens a new array padded out to its index (appendBuild).
	for _, k := range segs[1:] {
		if add(k) {
			return true
		}
	}
	return false
}

// digitsCanExceed reports whether path holds enough digit bytes for its
// indexes to sum past max: k digits can spell at most 10^k-1.
func digitsCanExceed(path string, max int) bool {
	most := 0 // 10^k - 1 for the digits seen so far
	for i := 0; i < len(path); i++ {
		if c := path[i]; c >= '0' && c <= '9' {
			if most = most*10 + 9; most > max {
				return true
			}
		}
	}
	return false
}

func arrayLen(arr gjson.Result) int {
	n := 0
	arr.ForEach(func(_, _ gjson.Result) bool { n++; return true })
	return n
}

// BoundedSet is sjson.Set for an author-supplied path: it returns doc
// unchanged and ErrArrayPad instead of padding an array past MaxArrayPad.
func BoundedSet(doc, path string, value any) (string, error) {
	if PadsArray(doc, path) {
		return doc, ErrArrayPad
	}
	return sjson.Set(doc, path, value)
}

// BoundedSetRaw is sjson.SetRaw with the same bound.
func BoundedSetRaw(doc, path, raw string) (string, error) {
	if PadsArray(doc, path) {
		return doc, ErrArrayPad
	}
	return sjson.SetRaw(doc, path, raw)
}

// BoundedSetBytes is sjson.SetBytes with the same bound.
func BoundedSetBytes(doc []byte, path string, value any) ([]byte, error) {
	if digitsCanExceed(path, MaxArrayPad) && PadsArray(string(doc), path) {
		return doc, ErrArrayPad
	}
	return sjson.SetBytes(doc, path, value)
}
