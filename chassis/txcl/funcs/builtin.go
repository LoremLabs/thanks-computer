package funcs

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// PR 3 shipped the pilots (&uuid, &now) end-to-end so the parser →
// runtime → registry pipeline could be validated with the smallest
// possible registry surface. PR 4 fills out the rest — 16 strict
// functions + 5 try_ safe variants. All registrations live in this
// init() so a reader can find every public function from one block.
//
// Discipline (see internal docs/todo-txcl-expressions.md §4):
//   - side-effect-free: no Unit access, no I/O, no bus dispatch
//   - synchronous: no goroutines, no sleeps
//   - safe for concurrent invocation
//   - well-defined error shape for the strict forms; the try_
//     wrappers (try.go) swallow errors → nil.

func init() {
	// pilots (shipped in PR 3, kept here for the full registry view)
	register("uuid", uuidFn)
	register("now", nowFn)

	// codecs
	register("b64encode", b64encodeFn)
	register("b64decode", b64decodeFn)
	register("urlencode", urlencodeFn)
	register("urldecode", urldecodeFn)
	register("json", jsonFn)
	register("to_json", toJSONFn)

	// JSON path access (runtime-computed paths)
	register("get", getFn)
	register("set", setFn)
	register("has", hasFn)

	// constructors
	register("object", objectFn)
	register("array", arrayFn)

	// string / hash utilities
	register("concat", concatFn)
	register("len", lenFn)
	register("split", splitFn)
	register("join", joinFn)
	register("substr", substrFn)
	register("sha256", sha256Fn)
}

// --- generators / time -----------------------------------------

// uuidFn returns a UUID v7 — time-ordered (sortable by creation
// time), 128 bits, formatted as the standard 8-4-4-4-12 hex string.
// v7 is preferred over v4 for IDs that benefit from chronological
// ordering (database keys, correlation IDs, log lookups).
func uuidFn(args []any) (any, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("&uuid: expected 0 arguments, got %d", len(args))
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("&uuid: %w", err)
	}
	return id.String(), nil
}

// nowFn returns the current wall-clock time. See the format selector
// switch for the supported representations.
func nowFn(args []any) (any, error) {
	now := time.Now()
	if len(args) == 0 {
		return now.Unix(), nil
	}
	if len(args) > 1 {
		return nil, fmt.Errorf("&now: expected 0 or 1 argument, got %d", len(args))
	}
	fmtSel, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("&now: format argument must be a string, got %T", args[0])
	}
	switch fmtSel {
	case "", "unix":
		return now.Unix(), nil
	case "millis":
		return now.UnixMilli(), nil
	case "nanos":
		return now.UnixNano(), nil
	case "rfc3339", "iso8601":
		return now.UTC().Format(time.RFC3339), nil
	default:
		return nil, fmt.Errorf("&now: unknown format %q (want unix|millis|nanos|rfc3339)", fmtSel)
	}
}

// --- codecs ----------------------------------------------------

func b64encodeFn(args []any) (any, error) {
	s, err := arg1String("&b64encode", args)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.EncodeToString([]byte(s)), nil
}

func b64decodeFn(args []any) (any, error) {
	s, err := arg1String("&b64decode", args)
	if err != nil {
		return nil, err
	}
	b, derr := base64.StdEncoding.DecodeString(s)
	if derr != nil {
		return nil, fmt.Errorf("&b64decode: %w", derr)
	}
	return string(b), nil
}

func urlencodeFn(args []any) (any, error) {
	s, err := arg1String("&urlencode", args)
	if err != nil {
		return nil, err
	}
	// QueryEscape is the right behavior for query strings and form
	// values. PathEscape leaves more characters alone (`/`) and isn't
	// what most callers want when they reach for `urlencode`; we can
	// add &pathencode later if the case shows up.
	return url.QueryEscape(s), nil
}

func urldecodeFn(args []any) (any, error) {
	s, err := arg1String("&urldecode", args)
	if err != nil {
		return nil, err
	}
	out, derr := url.QueryUnescape(s)
	if derr != nil {
		return nil, fmt.Errorf("&urldecode: %w", derr)
	}
	return out, nil
}

// jsonFn parses a JSON string into a Go value (map / array / scalar
// / nil). Returns the unmarshaled value so downstream addressing
// via @-paths or &get works naturally — sjson serializes the
// returned structure when the value lands in the envelope.
func jsonFn(args []any) (any, error) {
	s, err := arg1String("&json", args)
	if err != nil {
		return nil, err
	}
	if s == "" {
		// json.Unmarshal of "" errors with "unexpected end of JSON
		// input"; surface a clearer message for the common case.
		return nil, fmt.Errorf("&json: empty input")
	}
	var v any
	if jerr := json.Unmarshal([]byte(s), &v); jerr != nil {
		return nil, fmt.Errorf("&json: %w", jerr)
	}
	return v, nil
}

// toJSONFn serializes any value to a compact JSON string.
// json.Marshal handles all the shapes the runtime can produce
// (strings, numbers, bools, nil, slices, maps); anything that
// fails marshaling is a programming error and gets surfaced.
func toJSONFn(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("&to_json: expected 1 argument, got %d", len(args))
	}
	b, err := json.Marshal(args[0])
	if err != nil {
		return nil, fmt.Errorf("&to_json: %w", err)
	}
	return string(b), nil
}

// --- JSON path access ------------------------------------------

// getFn walks `obj` by gjson path. Returns nil on missing path
// (not an error — absence is semantically "not present"); errors
// only when `obj` itself isn't walkable (e.g. a number, where any
// path is nonsensical).
func getFn(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("&get: expected 2 arguments (obj, path), got %d", len(args))
	}
	path, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("&get: path argument must be a string, got %T", args[1])
	}
	js, err := objAsJSON(args[0])
	if err != nil {
		return nil, fmt.Errorf("&get: %w", err)
	}
	r := gjson.Get(js, path)
	if !r.Exists() {
		return nil, nil
	}
	return r.Value(), nil
}

// setFn writes `value` at `path` inside `obj` and returns the
// modified structure as an unmarshaled Go value. Like getFn it
// goes through a JSON round-trip; this is the simplest correct
// implementation and matches PR 4's "correct, not optimized"
// posture.
func setFn(args []any) (any, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("&set: expected 3 arguments (obj, path, value), got %d", len(args))
	}
	path, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("&set: path argument must be a string, got %T", args[1])
	}
	js, err := objAsJSON(args[0])
	if err != nil {
		return nil, fmt.Errorf("&set: %w", err)
	}
	out, serr := sjson.Set(js, path, args[2])
	if serr != nil {
		return nil, fmt.Errorf("&set: %w", serr)
	}
	var v any
	if uerr := json.Unmarshal([]byte(out), &v); uerr != nil {
		return nil, fmt.Errorf("&set: %w", uerr)
	}
	return v, nil
}

// hasFn returns whether `path` exists inside `obj`. The companion
// to &get for the case where you want to distinguish "absent" from
// "present but null." Type errors on the obj argument fail loud —
// same posture as &get.
func hasFn(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("&has: expected 2 arguments (obj, path), got %d", len(args))
	}
	path, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("&has: path argument must be a string, got %T", args[1])
	}
	js, err := objAsJSON(args[0])
	if err != nil {
		return nil, fmt.Errorf("&has: %w", err)
	}
	return gjson.Get(js, path).Exists(), nil
}

// --- constructors ----------------------------------------------

// objectFn assembles a map from interleaved key-value args.
// Semantics (per design doc §4):
//   - odd arg count → halt (key without value is a rule bug)
//   - non-string key → halt (object keys must be strings)
//   - duplicate key → last-wins (right-most pair)
func objectFn(args []any) (any, error) {
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("&object: expected an even number of arguments (key, value, ...), got %d", len(args))
	}
	out := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			return nil, fmt.Errorf("&object: key at position %d must be a string, got %T", i, args[i])
		}
		out[key] = args[i+1] // last-wins on duplicate
	}
	return out, nil
}

// arrayFn assembles a slice from its args. Variadic; zero args
// returns an empty []any.
func arrayFn(args []any) (any, error) {
	// Copy to a new slice so callers can't mutate the funcs internal
	// state via the returned value (paranoid; args is itself a
	// fresh slice from runtime.Resolve, but the copy makes the
	// ownership obvious).
	out := make([]any, len(args))
	copy(out, args)
	return out, nil
}

// --- string / hash utilities -----------------------------------

// concatFn joins its args into a single string. Strings pass
// through; other scalar values are coerced via fmt.Sprintf("%v").
// The coerce vs strict tradeoff: strict is more honest but forces
// authors to write `&concat(@a, "-", &to_json(@n))` when they just
// want "@a-3"; coercion is the ergonomic default.
func concatFn(args []any) (any, error) {
	var b strings.Builder
	for _, a := range args {
		switch v := a.(type) {
		case string:
			b.WriteString(v)
		case nil:
			// Treat nil as empty — natural for "concat these fields
			// even if some are missing" idioms.
		default:
			b.WriteString(fmt.Sprintf("%v", v))
		}
	}
	return b.String(), nil
}

// lenFn returns the length of a string, array, or map. Other types
// halt — len of a number or bool is undefined.
func lenFn(args []any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("&len: expected 1 argument, got %d", len(args))
	}
	switch v := args[0].(type) {
	case string:
		return int64(len(v)), nil
	case []any:
		return int64(len(v)), nil
	case map[string]any:
		return int64(len(v)), nil
	case nil:
		return int64(0), nil
	default:
		return nil, fmt.Errorf("&len: unsupported type %T (want string, array, object, or null)", v)
	}
}

// splitFn splits `s` on `sep` and returns the resulting parts as
// a []any (so the result mixes cleanly with other constructors
// like &array). Mirrors strings.Split semantics: empty `sep`
// splits into individual bytes.
func splitFn(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("&split: expected 2 arguments (s, sep), got %d", len(args))
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("&split: first argument must be a string, got %T", args[0])
	}
	sep, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("&split: separator must be a string, got %T", args[1])
	}
	parts := strings.Split(s, sep)
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out, nil
}

// joinFn joins an array's elements into a single string with `sep`
// between them — the inverse of &split. Elements are coerced to
// strings the way &concat coerces (strings pass through, nil → empty,
// other scalars via fmt.Sprintf). Halts if the first arg isn't an
// array (joining a non-array is a rule bug, not a recoverable input).
func joinFn(args []any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("&join: expected 2 arguments (array, sep), got %d", len(args))
	}
	arr, ok := args[0].([]any)
	if !ok {
		return nil, fmt.Errorf("&join: first argument must be an array, got %T", args[0])
	}
	sep, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("&join: separator must be a string, got %T", args[1])
	}
	parts := make([]string, len(arr))
	for i, a := range arr {
		switch v := a.(type) {
		case string:
			parts[i] = v
		case nil:
			parts[i] = ""
		default:
			parts[i] = fmt.Sprintf("%v", v)
		}
	}
	return strings.Join(parts, sep), nil
}

// substrFn returns s[start:end] (half-open, byte-indexed). Out-of-
// range indices halt; negative indices halt. Byte indexing is fine
// for ASCII; multi-byte UTF-8 may slice inside a rune — rune-aware
// indexing is a future call when a use case appears.
func substrFn(args []any) (any, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("&substr: expected 3 arguments (s, start, end), got %d", len(args))
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("&substr: first argument must be a string, got %T", args[0])
	}
	start, err := toInt("&substr start", args[1])
	if err != nil {
		return nil, err
	}
	end, err := toInt("&substr end", args[2])
	if err != nil {
		return nil, err
	}
	if start < 0 || end < 0 {
		return nil, fmt.Errorf("&substr: negative indices not supported (start=%d, end=%d)", start, end)
	}
	if start > end {
		return nil, fmt.Errorf("&substr: start (%d) > end (%d)", start, end)
	}
	if end > len(s) {
		return nil, fmt.Errorf("&substr: end (%d) exceeds string length (%d)", end, len(s))
	}
	return s[start:end], nil
}

// sha256Fn returns the lowercase hex digest of the SHA-256 hash of
// the input string. For binary input, base64-encode first or hash
// the b64 representation directly — the function is string-only by
// design (the chassis envelope holds JSON values, which are
// strings on the way in).
func sha256Fn(args []any) (any, error) {
	s, err := arg1String("&sha256", args)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:]), nil
}

// --- helpers ---------------------------------------------------

// arg1String validates the common 1-string-arg call shape used by
// &b64encode, &b64decode, &urlencode, &urldecode, &json, &sha256.
// Centralizes the error messages so they're consistent.
func arg1String(name string, args []any) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("%s: expected 1 argument, got %d", name, len(args))
	}
	s, ok := args[0].(string)
	if !ok {
		return "", fmt.Errorf("%s: argument must be a string, got %T", name, args[0])
	}
	return s, nil
}

// toInt coerces a numeric arg to int. JSON-marshaled numbers come
// back as float64 from json.Unmarshal; the parser produces int64
// for integer literals; both work as input. Anything else halts.
func toInt(name string, v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		// Tolerate fractional zero (e.g. JSON 3 → float64(3)) but
		// halt if the caller passed a non-integer.
		if n != float64(int(n)) {
			return 0, fmt.Errorf("%s: expected integer, got fractional %v", name, n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%s: must be a number, got %T", name, v)
	}
}

// objAsJSON normalizes an obj argument (for &get / &set / &has) to
// a JSON string that gjson/sjson can walk. Accepts nil, strings,
// and any json.Marshal-able value (maps, arrays, scalars).
//
// Subtlety: if `obj` is already a string, we treat it as either a
// JSON literal OR as a string-valued envelope leaf — gjson.Valid is
// the heuristic. This means `&get("hello", "anything")` errors via
// the gjson.Valid("hello") = false branch, which is the right
// outcome ("hello" isn't walkable). A caller that legitimately
// wants to walk a JSON-encoded string field can pass it as-is;
// gjson.Valid catches the true JSON case.
func objAsJSON(obj any) (string, error) {
	if obj == nil {
		return "null", nil
	}
	if s, ok := obj.(string); ok {
		if gjson.Valid(s) {
			return s, nil
		}
		return "", fmt.Errorf("string value is not valid JSON; cannot walk it")
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
