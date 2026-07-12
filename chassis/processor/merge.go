package processor

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MergeJSON folds dst's top-level keys into src: scalars overwrite,
// objects deep-merge, arrays append (dst elements after src — this is
// the accumulator design). The fast path builds the result in a single
// buffer; it bails to the sjson-based slow path on any input it cannot
// prove it reproduces byte-for-byte (odd keys, duplicate keys,
// whitespace-led docs, merge errors).
// Both paths MUST stay byte-identical: envelopes are golden-matched in
// tests and traces. See merge_oracle_test.go.
func (pu *Unit) MergeJSON(src string, dst string) (string, error) {
	if out, ok := mergeJSONFast(src, dst); ok {
		return out, nil
	}
	return pu.mergeJSONSlow(src, dst)
}

// deepMergeValues merges materialized JSON values with dst-wins scalar
// semantics and array append (src elements first). x1 is the dst-side
// value, x2 the src-side. Hoisted verbatim from the original MergeJSON
// closure — its quirks (nested dst array loses to src non-array) are
// load-bearing for byte-identity.
func deepMergeValues(x1, x2 interface{}) interface{} {
	switch x1 := x1.(type) {
	case map[string]interface{}:
		x2, ok := x2.(map[string]interface{})
		if !ok {
			return x1
		}
		for k, v2 := range x2 {
			if v1, ok := x1[k]; ok {
				x1[k] = deepMergeValues(v1, v2)
			} else {
				x1[k] = v2
			}
		}
	case nil:
		// merge(nil, map[string]interface{...}) -> map[string]interface{...}
		x2, ok := x2.(map[string]interface{})
		if ok {
			return x2
		}
	case []interface{}:
		x2, ok := x2.([]interface{})
		if ok {
			merged := append(x2, x1...)
			return merged
		}
		return x2
	}
	return x1
}

// mergeJSONSlow is the original sjson-per-key merge, kept verbatim as
// the semantic reference for the fast path and the fallback for inputs
// the fast path bails on.
func (pu *Unit) mergeJSONSlow(src string, dst string) (string, error) {
	// doesn't deep merge. maybe https://play.golang.org/p/8jlJUbEJKf ?

	// short circuit for empty strings
	if dst == "" {
		return src, nil
	}
	if src == "" {
		return dst, nil
	}

	d := gjson.Parse(dst)
	s := gjson.Parse(src)
	if !d.IsObject() || !s.IsObject() {
		return "", errors.New("Merging requires both sides to be objects")
	}

	var escapeDot = func(key string) string {
		s := strings.ReplaceAll(key, ".", "\\.")
		return strings.ReplaceAll(s, ":", "\\:")
	}

	var derr error
	d.ForEach(func(key, value gjson.Result) bool {
		// if the typeof the value is not object or map, just insert
		// otherwise, we merge insert
		switch value.Type {
		case gjson.JSON:
			// see if this object exists in both source and destination
			v := gjson.Get(src, escapeDot(key.String()))
			if !v.Exists() {
				// good news, we can just insert it
				switch {
				case value.IsObject():
					m, ok := value.Value().(map[string]interface{})
					if !ok {
						derr = errors.New("merge object error")
						return false
					}
					src, _ = sjson.Set(src, escapeDot(key.String()), m)
				case value.IsArray():
					m, ok := value.Value().([]interface{})
					if !ok {
						derr = errors.New("merge array error")
						return false
					}
					src, _ = sjson.Set(src, escapeDot(key.String()), m)
				}
			} else {
				// exists in both places, let's deepmerge, append arrays
				switch {
				case value.IsObject():
					d1, dok := value.Value().(map[string]interface{})
					s1, sok := v.Value().(map[string]interface{})
					if !dok || !sok {
						derr = errors.New("deepmerge obj error")

						return false
					}

					merged := deepMergeValues(d1, s1)
					src, _ = sjson.Set(src, escapeDot(key.String()), merged)
				case value.IsArray():
					d1, dok := value.Value().([]interface{})
					s1, sok := v.Value().([]interface{})
					if v.IsArray() {
						if !dok || !sok {
							derr = errors.New("deepmerge obj error")
							return false
						}
						// TODO: check to see if arrays v/value are the same size and if s1 == d1.
						// if it's the same, don't append.
						merged := append(s1, d1...)
						src, _ = sjson.Set(src, escapeDot(key.String()), merged)
					} else {
						src, _ = sjson.Set(src, escapeDot(key.String()), d1)
					}
				}
			}

		case gjson.String:
			src, _ = sjson.Set(src, escapeDot(key.String()), value.String())
		case gjson.False:
			src, _ = sjson.Set(src, escapeDot(key.String()), false)
		case gjson.True:
			src, _ = sjson.Set(src, escapeDot(key.String()), true)
		case gjson.Number:
			src, _ = sjson.Set(src, escapeDot(key.String()), value.Num)
		default:
			src, _ = sjson.Set(src, escapeDot(key.String()), nil)
		}

		return true // keep iterating
	})

	return src, derr
}

// plainMergeKey reports whether a top-level key is safe for the fast
// path: exact-match semantics under gjson/sjson (after escapeDot) with
// no wildcard/modifier/escape surprises. Anything else bails to the
// slow path.
func plainMergeKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.' || c == ':' || c == '/' || c == '$' || c == ' ':
		default:
			return false
		}
	}
	return true
}

// mergeEncodeScalar encodes a dst scalar exactly as the slow path's
// sjson.Set would: strings via appendStringify, numbers via
// FormatFloat(value.Num, 'f', -1, 64), null via encoding/json of nil.
func mergeEncodeScalar(value gjson.Result) ([]byte, bool) {
	switch value.Type {
	case gjson.String:
		return jsonx.AppendStringify(nil, value.Str), true
	case gjson.True:
		return []byte("true"), true
	case gjson.False:
		return []byte("false"), true
	case gjson.Number:
		return strconv.AppendFloat(nil, value.Num, 'f', -1, 64), true
	case gjson.Null:
		return []byte("null"), true
	}
	return nil, false
}

type mergeReplace struct {
	start, end int
	raw        []byte
}

// mergeJSONFast is the single-buffer merge. It returns ok=false to
// fall back to mergeJSONSlow whenever byte-identity is not provable.
func mergeJSONFast(src, dst string) (string, bool) {
	if src == "" || dst == "" {
		return "", false
	}
	// Only src bytes are spliced into the output, so only src needs
	// the strict gates: sjson drops leading whitespace on insert but
	// keeps it on replace, and gjson tolerates trailing garbage that
	// sjson's Parse().Raw would truncate — a '{' first byte plus
	// strict validity sidesteps both. dst is never copied verbatim;
	// it is materialized through the exact same Parse/ForEach/Value
	// calls the slow path makes, so dst oddities hit both paths
	// identically.
	if src[0] != '{' || dst[0] != '{' {
		return "", false
	}
	if !gjson.Valid(src) {
		return "", false
	}
	s := gjson.Parse(src)
	d := gjson.Parse(dst)
	if !s.IsObject() || !d.IsObject() {
		return "", false
	}

	srcVals := make(map[string]gjson.Result)
	ok := true
	s.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if !plainMergeKey(k) {
			ok = false
			return false
		}
		if _, dup := srcVals[k]; dup {
			ok = false
			return false
		}
		if value.Index <= 0 || value.Index+len(value.Raw) > len(src) ||
			src[value.Index:value.Index+len(value.Raw)] != value.Raw {
			ok = false
			return false
		}
		srcVals[k] = value
		return true
	})
	if !ok {
		return "", false
	}

	var replaces []mergeReplace
	var prepends [][]byte
	prependLen := 0
	dstSeen := make(map[string]struct{})
	d.ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if !plainMergeKey(k) {
			ok = false
			return false
		}
		if _, dup := dstSeen[k]; dup {
			// second write to the same key is order-dependent
			ok = false
			return false
		}
		dstSeen[k] = struct{}{}

		// gjson.Get/sjson.Set are exact-match only (verified
		// empirically: no case-insensitive fallback in this gjson
		// version), so the map lookup reproduces the slow path's
		// key resolution for plain unique keys.
		sv, exists := srcVals[k]

		var raw []byte
		switch value.Type {
		case gjson.JSON:
			var goVal interface{}
			switch {
			case value.IsObject():
				d1, dok := value.Value().(map[string]interface{})
				if !dok {
					ok = false
					return false
				}
				if exists {
					s1, sok := sv.Value().(map[string]interface{})
					if !sok {
						// slow path errors here ("deepmerge obj
						// error") and returns a partial doc
						ok = false
						return false
					}
					goVal = deepMergeValues(d1, s1)
				} else {
					goVal = d1
				}
			case value.IsArray():
				d1, dok := value.Value().([]interface{})
				if !dok {
					ok = false
					return false
				}
				if exists && sv.IsArray() {
					s1, sok := sv.Value().([]interface{})
					if !sok {
						ok = false
						return false
					}
					goVal = append(s1, d1...)
				} else {
					goVal = d1
				}
			default:
				ok = false
				return false
			}
			b, merr := json.Marshal(goVal)
			if merr != nil {
				ok = false
				return false
			}
			raw = b
		default:
			var sok bool
			raw, sok = mergeEncodeScalar(value)
			if !sok {
				ok = false
				return false
			}
		}

		if exists {
			replaces = append(replaces, mergeReplace{sv.Index, sv.Index + len(sv.Raw), raw})
		} else {
			var pb []byte
			pb = jsonx.AppendStringify(pb, k)
			pb = append(pb, ':')
			pb = append(pb, raw...)
			prepends = append(prepends, pb)
			prependLen += len(pb) + 1
		}
		return true
	})
	if !ok {
		return "", false
	}
	if len(replaces) == 0 && len(prepends) == 0 {
		return src, true
	}

	// sjson adds a comma after an inserted pair iff the object it
	// spliced into was non-empty (first non-ws byte after '{' != '}').
	srcNonEmpty := false
	for i := 1; i < len(src); i++ {
		if src[i] <= ' ' {
			continue
		}
		srcNonEmpty = src[i] != '}'
		break
	}

	// replaces arrive in dst order; emit needs src position order
	sort.Slice(replaces, func(i, j int) bool { return replaces[i].start < replaces[j].start })

	deltaGuess := 0
	for _, r := range replaces {
		deltaGuess += len(r.raw) - (r.end - r.start)
	}
	var b strings.Builder
	grow := len(src) + prependLen + deltaGuess + 1
	if grow < len(src) {
		grow = len(src)
	}
	b.Grow(grow)
	b.WriteByte('{')
	// sequential sjson.Set prepends compose to reverse insertion order
	for i := len(prepends) - 1; i >= 0; i-- {
		if i != len(prepends)-1 {
			b.WriteByte(',')
		}
		b.Write(prepends[i])
	}
	if len(prepends) > 0 && srcNonEmpty {
		b.WriteByte(',')
	}
	pos := 1
	for _, r := range replaces {
		b.WriteString(src[pos:r.start])
		b.Write(r.raw)
		pos = r.end
	}
	b.WriteString(src[pos:])
	return b.String(), true
}
