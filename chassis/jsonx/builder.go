// Package jsonx builds JSON documents with the exact byte-level
// semantics of a chain of sjson v1.0.4 Set/SetRaw calls — new keys
// PREPEND (so sequential inserts land in reverse order), existing
// values replace in place, "-1" appends to arrays, numeric indexes
// null-pad — but serialized ONCE instead of re-copying the whole
// document per call. Envelope byte-identity (golden tests, traces)
// depends on these semantics; sjson is pinned at v1.0.4 in go.mod for
// the same reason.
//
// Error semantics also mirror the ubiquitous `doc, _ = sjson.Set(...)`
// call shape: an invalid path (unescaped *?#, empty path) or an
// unmarshalable value poisons the document to "" and later Sets
// rebuild from empty, exactly like the chained form.
//
// Limitations (enforced by convention, exercised by the equivalence
// fuzz): SetRaw values must be valid JSON — descending a path *through*
// an invalid raw blob is undefined; and a Builder is not safe for
// concurrent use.
package jsonx

import (
	"encoding/json"
	"strconv"

	"github.com/tidwall/sjson"
)

const (
	kindRaw = iota
	kindObj
	kindArr
)

type node struct {
	kind int
	raw  string  // kindRaw: verbatim JSON bytes
	obj  []entry // kindObj: ordered
	arr  []*node // kindArr: ordered
}

type entry struct {
	key  string
	qkey string // key encoded as a JSON string (sjson appendStringify)
	val  *node
}

// Builder accumulates Set/SetRaw operations into a node tree and
// serializes once. The zero value / New() is the empty document ""
// (matching a chain that starts from ""); NewObject() matches a chain
// that starts from "{}" — the two differ when the first path segment
// is numeric (sjson turns an empty doc into an array, but inserts a
// string key into "{}").
type Builder struct {
	root *node
}

func New() *Builder { return &Builder{} }

func NewObject() *Builder { return &Builder{root: &node{kind: kindObj}} }

// NewArray starts from "[]" (the `sjson.Set("[]", "-1", ...)` append
// seed used by list builders).
func NewArray() *Builder { return &Builder{root: &node{kind: kindArr}} }

// Set encodes v exactly as sjson.Set would (strings and []byte via
// appendStringify, sized ints via FormatInt, float32 via the same
// widen-then-'f' quirk, everything else via encoding/json) and applies
// it at path.
func (b *Builder) Set(path string, v any) {
	raw, ok := encodeValue(v)
	if !ok {
		b.root = nil // json.Marshal error → `doc, _ =` poisons to ""
		return
	}
	b.apply(path, raw)
}

// SetRaw splices raw (which must be valid JSON) at path verbatim.
func (b *Builder) SetRaw(path, raw string) {
	b.apply(path, raw)
}

func (b *Builder) apply(path, raw string) {
	segs, ok := splitPath(path)
	if !ok {
		b.root = nil
		return
	}
	root, ok := setNode(b.root, segs, raw)
	if !ok {
		b.root = nil
		return
	}
	b.root = root
}

// String serializes the document. An empty (or poisoned) Builder
// returns "", matching a zero-op / errored sjson chain.
func (b *Builder) String() string {
	if b.root == nil {
		return ""
	}
	buf := make([]byte, 0, sizeNode(b.root))
	buf = writeNode(buf, b.root)
	return string(buf)
}

type pathSeg struct {
	name  string
	force bool // ':' prefix — force string-key semantics
}

// splitPath mirrors sjson v1.0.4 parsePath: '.' separates, '\' escapes
// the next byte, a leading ':' per segment sets force, and unescaped
// '*' '?' '#' (or an empty path) are errors.
func splitPath(path string) ([]pathSeg, bool) {
	if path == "" {
		return nil, false
	}
	var segs []pathSeg
	var cur []byte
	force := false
	segStart := true
	for i := 0; i < len(path); i++ {
		c := path[i]
		if segStart && c == ':' {
			force = true
			segStart = false
			continue
		}
		segStart = false
		switch c {
		case '\\':
			i++
			if i < len(path) {
				cur = append(cur, path[i])
			}
		case '.':
			segs = append(segs, pathSeg{string(cur), force})
			cur = cur[:0]
			force = false
			segStart = true
		case '*', '?', '#':
			return nil, false
		default:
			cur = append(cur, c)
		}
	}
	segs = append(segs, pathSeg{string(cur), force})
	return segs, true
}

// joinSegs re-encodes segments into an sjson path (used when
// delegating a descend-into-raw-leaf to real sjson).
func joinSegs(segs []pathSeg) string {
	var b []byte
	for i, s := range segs {
		if i > 0 {
			b = append(b, '.')
		}
		if s.force {
			b = append(b, ':')
		}
		for j := 0; j < len(s.name); j++ {
			switch s.name[j] {
			case '\\', '.', '*', '?', '#', ':':
				b = append(b, '\\')
			}
			b = append(b, s.name[j])
		}
	}
	return string(b)
}

// gjsonArrayIndex reports whether name is a gjson-matchable array
// index: non-empty, digits only (leading zeros allowed).
func gjsonArrayIndex(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return 0, false
		}
		n = n*10 + int(name[i]-'0')
	}
	return n, true
}

// atouiSeg is sjson's atoui verbatim, including the quirk that an
// empty segment parses as numeric 0 and overflow wraps silently.
func atouiSeg(s pathSeg) (int, bool) {
	if s.force {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s.name); i++ {
		if s.name[i] < '0' || s.name[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s.name[i]-'0')
	}
	return n, true
}

func leaf(raw string) *node { return &node{kind: kindRaw, raw: raw} }

func mkEntry(key string, val *node) entry {
	return entry{key: key, qkey: string(AppendStringify(nil, key)), val: val}
}

// createValue builds the value for a freshly-created path per sjson's
// appendBuild: a numeric next segment makes an array padded with n
// nulls, "-1" makes a single-element array, anything else an object.
func createValue(segs []pathSeg, raw string) *node {
	if len(segs) == 0 {
		return leaf(raw)
	}
	s := segs[0]
	if n, numeric := atouiSeg(s); numeric {
		a := &node{kind: kindArr}
		for i := 0; i < n; i++ {
			a.arr = append(a.arr, leaf("null"))
		}
		a.arr = append(a.arr, createValue(segs[1:], raw))
		return a
	}
	if !s.force && s.name == "-1" {
		return &node{kind: kindArr, arr: []*node{createValue(segs[1:], raw)}}
	}
	return &node{kind: kindObj, obj: []entry{mkEntry(s.name, createValue(segs[1:], raw))}}
}

// createRoot handles the empty-document case, which differs from
// nested creation: sjson only turns an empty doc into an array for a
// NUMERIC first segment ("-1" gets a string key in a fresh object).
func createRoot(segs []pathSeg, raw string) *node {
	if _, numeric := atouiSeg(segs[0]); numeric {
		return createValue(segs, raw)
	}
	return &node{kind: kindObj, obj: []entry{mkEntry(segs[0].name, createValue(segs[1:], raw))}}
}

func setNode(n *node, segs []pathSeg, raw string) (*node, bool) {
	if n == nil {
		return createRoot(segs, raw), true
	}
	if len(segs) == 0 {
		return leaf(raw), true
	}
	s := segs[0]
	switch n.kind {
	case kindObj:
		for i := range n.obj {
			if n.obj[i].key == s.name {
				v, ok := setNode(n.obj[i].val, segs[1:], raw)
				if !ok {
					return nil, false
				}
				n.obj[i].val = v
				return n, true
			}
		}
		// missing key: sjson PREPENDS the new pair
		n.obj = append([]entry{mkEntry(s.name, createValue(segs[1:], raw))}, n.obj...)
		return n, true
	case kindArr:
		// The hit test mirrors gjson's array indexing (which sjson
		// uses for lookup): non-empty digits-only — leading zeros OK,
		// "" misses, and the ':' force flag is NOT consulted. Only on
		// a miss does sjson's own insert logic (atoui + force) apply,
		// and there the new value always lands at the END, padded
		// with nulls only when the index exceeds the length (so "" —
		// numeric 0 to atoui but invisible to gjson — appends).
		if idx, isIdx := gjsonArrayIndex(s.name); isIdx && idx >= 0 && idx < len(n.arr) {
			v, ok := setNode(n.arr[idx], segs[1:], raw)
			if !ok {
				return nil, false
			}
			n.arr[idx] = v
			return n, true
		}
		if idx, numeric := atouiSeg(s); numeric {
			for len(n.arr) < idx {
				n.arr = append(n.arr, leaf("null"))
			}
			n.arr = append(n.arr, createValue(segs[1:], raw))
			return n, true
		}
		if !s.force && s.name == "-1" {
			n.arr = append(n.arr, createValue(segs[1:], raw))
			return n, true
		}
		// "cannot set array element for non-numeric key"
		return nil, false
	default: // kindRaw
		// Descending through a raw leaf (a SetRaw'd blob or scalar):
		// delegate to real sjson on the leaf's bytes — identical by
		// construction, and rare at the converted call sites.
		sub, err := sjson.SetRawOptions(n.raw, joinSegs(segs), raw, nil)
		if err != nil {
			return nil, false
		}
		return leaf(sub), true
	}
}

func sizeNode(n *node) int {
	switch n.kind {
	case kindObj:
		sz := 2
		for i := range n.obj {
			if i > 0 {
				sz++
			}
			sz += len(n.obj[i].qkey) + 1 + sizeNode(n.obj[i].val)
		}
		return sz
	case kindArr:
		sz := 2
		for i, e := range n.arr {
			if i > 0 {
				sz++
			}
			sz += sizeNode(e)
		}
		return sz
	default:
		return len(n.raw)
	}
}

func writeNode(buf []byte, n *node) []byte {
	switch n.kind {
	case kindObj:
		buf = append(buf, '{')
		for i := range n.obj {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, n.obj[i].qkey...)
			buf = append(buf, ':')
			buf = writeNode(buf, n.obj[i].val)
		}
		return append(buf, '}')
	case kindArr:
		buf = append(buf, '[')
		for i, e := range n.arr {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = writeNode(buf, e)
		}
		return append(buf, ']')
	default:
		return append(buf, n.raw...)
	}
}

// MustMarshalString / AppendStringify replicate sjson v1.0.4's string
// encoder (MIT, github.com/tidwall/sjson): fast-quote when every byte
// is plain printable ASCII, else encoding/json (which HTML-escapes).
func MustMarshalString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > 0x7f || s[i] == '"' || s[i] == '\\' {
			return true
		}
	}
	return false
}

func AppendStringify(buf []byte, s string) []byte {
	if MustMarshalString(s) {
		b, _ := json.Marshal(s)
		return append(buf, b...)
	}
	buf = append(buf, '"')
	buf = append(buf, s...)
	buf = append(buf, '"')
	return buf
}

// encodeValue mirrors sjson v1.0.4 SetBytesOptions' type switch
// exactly. Notably float32 widens to float64 before 'f' formatting
// (NOT shortest-float32 like encoding/json), and plain int/uint fall
// through to encoding/json there — identical digits to FormatInt, used
// here directly.
func encodeValue(v any) (string, bool) {
	switch v := v.(type) {
	case nil:
		return "null", true
	case string:
		return string(AppendStringify(nil, v)), true
	case []byte:
		return string(AppendStringify(nil, string(v))), true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	case int:
		return strconv.FormatInt(int64(v), 10), true
	case int8:
		return strconv.FormatInt(int64(v), 10), true
	case int16:
		return strconv.FormatInt(int64(v), 10), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint:
		return strconv.FormatUint(uint64(v), 10), true
	case uint8:
		return strconv.FormatUint(uint64(v), 10), true
	case uint16:
		return strconv.FormatUint(uint64(v), 10), true
	case uint32:
		return strconv.FormatUint(uint64(v), 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}
