package funcs

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // embed zoneinfo so &tz tests resolve zones on minimal CI
)

// --- dispatcher --------------------------------------------------

func TestCall_Unknown(t *testing.T) {
	v, err := Call("definitely-not-real", nil)
	if err == nil {
		t.Fatalf("expected error for unknown function, got val %v", v)
	}
	if v != nil {
		t.Fatalf("expected nil value on unknown, got %v", v)
	}
	if !strings.Contains(err.Error(), "&definitely-not-real") {
		t.Errorf("error should mention function name, got %v", err)
	}
}

func TestRegistryHas(t *testing.T) {
	// Smoke for a few representative names from each category. The
	// registry is small enough that exhaustive coverage would be
	// noise; this catches "init dropped a registration" regressions.
	want := []string{
		"uuid", "now", "tz",
		"b64encode", "b64decode", "urlencode", "urldecode", "json", "to_json",
		"get", "set", "has",
		"object", "array",
		"concat", "len", "split", "join", "substr", "sha256",
		"try_json", "try_b64decode", "try_urldecode", "try_get", "try_substr",
	}
	for _, name := range want {
		if !Has(name) {
			t.Errorf("Has(%q) = false; want true", name)
		}
	}
	if Has("nope") {
		t.Errorf("Has(\"nope\") = true; want false")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	// Re-registering an existing name is a programming bug; the
	// registry panics so the collision surfaces at process start
	// rather than as a silent overwrite later.
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate register")
		}
	}()
	register("uuid", uuidFn) // already registered in init()
}

// --- &tz ---------------------------------------------------------

func TestTz(t *testing.T) {
	// Non-DST zones → deterministic regardless of the date time.Now()
	// lands on (Japan and India never observe DST).
	cases := []struct {
		zone      string
		component string
		hour      int
		minute    int // local minute; -1 = omit the 4th arg (defaults 0)
		want      int64
	}{
		{"UTC", "hour", 9, -1, 9},
		{"UTC", "minute", 9, -1, 0},
		{"Asia/Tokyo", "hour", 9, -1, 0},      // 09:00 JST = 00:00 UTC
		{"Asia/Tokyo", "minute", 9, -1, 0},    // whole-hour offset → :00
		{"Asia/Tokyo", "hour", 0, -1, 15},     // 00:00 JST = 15:00 UTC (prior day)
		{"Asia/Kolkata", "hour", 9, -1, 3},    // 09:00 IST = 03:30 UTC → hour 3
		{"Asia/Kolkata", "minute", 9, -1, 30}, // …and minute 30 (the half-hour)
		{"Asia/Kolkata", "hour", 9, 45, 4},    // 09:45 IST = 04:15 UTC → hour 4
		{"Asia/Kolkata", "minute", 9, 45, 15}, // …and minute 15
	}
	for _, c := range cases {
		args := []any{c.zone, c.component, int64(c.hour)}
		if c.minute >= 0 {
			args = append(args, int64(c.minute))
		}
		got, err := Call("tz", args)
		if err != nil {
			t.Fatalf("&tz(%v) error: %v", args, err)
		}
		if got != c.want {
			t.Errorf("&tz(%v) = %v, want %d", args, got, c.want)
		}
	}
}

func TestTz_Errors(t *testing.T) {
	bad := [][]any{
		{},                       // too few args
		{"UTC", "hour"},          // too few args
		{"UTC", "hour", 9, 0, 0}, // too many args
		{123, "hour", 9},         // non-string zone
		{"UTC", 9, 9},            // non-string component
		{"UTC", "second", 9},     // bad component
		{"Not/AZone", "hour", 9}, // unknown zone
		{"UTC", "hour", 24},      // hour out of range
		{"UTC", "hour", -1},      // hour out of range
		{"UTC", "minute", 9, 60}, // minute out of range
	}
	for _, args := range bad {
		if v, err := Call("tz", args); err == nil {
			t.Errorf("&tz(%v) expected error, got %v", args, v)
		}
	}
}

// --- &uuid -------------------------------------------------------

var uuidV7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUuid_Shape(t *testing.T) {
	v, err := Call("uuid", nil)
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("uuid: got %T, want string", v)
	}
	if !uuidV7Pattern.MatchString(s) {
		t.Errorf("uuid: %q is not a v7-shaped UUID", s)
	}
}

func TestUuid_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 256)
	for i := 0; i < 256; i++ {
		v, err := Call("uuid", nil)
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		s := v.(string)
		if _, dup := seen[s]; dup {
			t.Fatalf("uuid: duplicate %q in run of 256", s)
		}
		seen[s] = struct{}{}
	}
}

func TestUuid_Sortable(t *testing.T) {
	// v7 encodes the timestamp in the leading 48 bits, so a
	// chronological burst should sort in creation order. This is the
	// whole point of choosing v7 over v4.
	const n = 32
	ids := make([]string, n)
	for i := range ids {
		v, _ := Call("uuid", nil)
		ids[i] = v.(string)
		time.Sleep(1 * time.Millisecond) // ensure distinct ms ticks
	}
	for i := 1; i < n; i++ {
		if ids[i-1] >= ids[i] {
			t.Errorf("uuid %d (%q) should sort before uuid %d (%q)",
				i-1, ids[i-1], i, ids[i])
		}
	}
}

func TestUuid_RejectsArgs(t *testing.T) {
	v, err := Call("uuid", []any{"extra"})
	if err == nil {
		t.Fatalf("expected error for extra args, got %v", v)
	}
}

// --- &now --------------------------------------------------------

func TestNow_DefaultUnix(t *testing.T) {
	before := time.Now().Unix()
	v, err := Call("now", nil)
	if err != nil {
		t.Fatalf("now: %v", err)
	}
	after := time.Now().Unix()

	got, ok := v.(int64)
	if !ok {
		t.Fatalf("now: got %T, want int64", v)
	}
	if got < before || got > after {
		t.Errorf("now: %d not within [%d, %d]", got, before, after)
	}
}

func TestNow_Formats(t *testing.T) {
	cases := []struct {
		fmt  string
		want func(any) bool
	}{
		{"unix", func(v any) bool { _, ok := v.(int64); return ok }},
		{"millis", func(v any) bool {
			n, ok := v.(int64)
			return ok && n > time.Now().Add(-time.Second).UnixMilli()
		}},
		{"nanos", func(v any) bool {
			n, ok := v.(int64)
			return ok && n > time.Now().Add(-time.Second).UnixNano()
		}},
		{"rfc3339", func(v any) bool {
			s, ok := v.(string)
			if !ok {
				return false
			}
			_, err := time.Parse(time.RFC3339, s)
			return err == nil
		}},
		{"iso8601", func(v any) bool {
			s, ok := v.(string)
			if !ok {
				return false
			}
			_, err := time.Parse(time.RFC3339, s)
			return err == nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.fmt, func(t *testing.T) {
			v, err := Call("now", []any{tc.fmt})
			if err != nil {
				t.Fatalf("now(%q): %v", tc.fmt, err)
			}
			if !tc.want(v) {
				t.Errorf("now(%q): %v failed shape check", tc.fmt, v)
			}
		})
	}
}

func TestNow_RejectsBadFormat(t *testing.T) {
	v, err := Call("now", []any{"bogus"})
	if err == nil {
		t.Fatalf("expected error for unknown format, got %v", v)
	}
}

func TestNow_RejectsNonStringArg(t *testing.T) {
	v, err := Call("now", []any{42})
	if err == nil {
		t.Fatalf("expected error for non-string arg, got %v", v)
	}
}

func TestNow_RejectsTooManyArgs(t *testing.T) {
	v, err := Call("now", []any{"unix", "extra"})
	if err == nil {
		t.Fatalf("expected error for extra args, got %v", v)
	}
}

// --- codecs ------------------------------------------------------

func TestB64encode(t *testing.T) {
	v, err := Call("b64encode", []any{"hello"})
	if err != nil {
		t.Fatalf("b64encode: %v", err)
	}
	if v != "aGVsbG8=" {
		t.Errorf("b64encode(hello): got %q, want %q", v, "aGVsbG8=")
	}
}

func TestB64decode_HappyPath(t *testing.T) {
	v, err := Call("b64decode", []any{"aGVsbG8="})
	if err != nil {
		t.Fatalf("b64decode: %v", err)
	}
	if v != "hello" {
		t.Errorf("b64decode: got %q, want %q", v, "hello")
	}
}

func TestB64decode_BadInput(t *testing.T) {
	v, err := Call("b64decode", []any{"not-valid-base64!@#$"})
	if err == nil {
		t.Fatalf("expected error for bad base64, got %v", v)
	}
}

func TestB64_Roundtrip(t *testing.T) {
	original := "hello, world\nwith\ttabs and \"quotes\""
	enc, err := Call("b64encode", []any{original})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := Call("b64decode", []any{enc})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec != original {
		t.Errorf("roundtrip: got %q, want %q", dec, original)
	}
}

func TestUrlencode(t *testing.T) {
	v, err := Call("urlencode", []any{"hello world & friends"})
	if err != nil {
		t.Fatalf("urlencode: %v", err)
	}
	if v != "hello+world+%26+friends" {
		t.Errorf("urlencode: got %q", v)
	}
}

func TestUrldecode_HappyPath(t *testing.T) {
	v, err := Call("urldecode", []any{"hello+world+%26+friends"})
	if err != nil {
		t.Fatalf("urldecode: %v", err)
	}
	if v != "hello world & friends" {
		t.Errorf("urldecode: got %q", v)
	}
}

func TestUrldecode_BadInput(t *testing.T) {
	// `%` followed by non-hex is invalid percent-encoding.
	v, err := Call("urldecode", []any{"%zz"})
	if err == nil {
		t.Fatalf("expected error for bad URL encoding, got %v", v)
	}
}

func TestJson_ParseObject(t *testing.T) {
	v, err := Call("json", []any{`{"a":1,"b":[true,"x"]}`})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("json: got %T, want map[string]any", v)
	}
	if m["a"] != float64(1) {
		t.Errorf("json.a: got %v, want 1", m["a"])
	}
	arr, ok := m["b"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("json.b: got %T %v", m["b"], m["b"])
	}
}

func TestJson_BadInput(t *testing.T) {
	v, err := Call("json", []any{"not json"})
	if err == nil {
		t.Fatalf("expected error, got %v", v)
	}
}

func TestJson_Empty(t *testing.T) {
	v, err := Call("json", []any{""})
	if err == nil {
		t.Fatalf("expected error for empty input, got %v", v)
	}
}

func TestToJson(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "hello", `"hello"`},
		{"int", int64(42), `42`},
		{"map", map[string]any{"a": 1}, `{"a":1}`},
		{"array", []any{"x", "y"}, `["x","y"]`},
		{"nil", nil, `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Call("to_json", []any{tc.in})
			if err != nil {
				t.Fatalf("to_json: %v", err)
			}
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

// --- JSON path access --------------------------------------------

func TestGet_Hit(t *testing.T) {
	obj := map[string]any{"a": map[string]any{"b": "found"}}
	v, err := Call("get", []any{obj, "a.b"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != "found" {
		t.Errorf("got %v, want 'found'", v)
	}
}

func TestGet_MissingReturnsNil(t *testing.T) {
	// Per design doc §4: missing path returns nil, not an error.
	obj := map[string]any{"a": "x"}
	v, err := Call("get", []any{obj, "missing.path"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != nil {
		t.Errorf("missing path should return nil, got %v", v)
	}
}

func TestGet_OnJSONString(t *testing.T) {
	// Passing a JSON string as obj works — gjson walks it directly.
	v, err := Call("get", []any{`{"x":42}`, "x"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v != float64(42) {
		t.Errorf("got %v, want 42", v)
	}
}

func TestGet_OnNonJSONString_Errors(t *testing.T) {
	// A plain (non-JSON) string isn't walkable. Strict form halts.
	v, err := Call("get", []any{"hello", "x"})
	if err == nil {
		t.Fatalf("expected error, got %v", v)
	}
}

func TestGet_OnNil(t *testing.T) {
	v, err := Call("get", []any{nil, "x"})
	if err != nil {
		t.Fatalf("get on nil: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestGet_RejectsNonStringPath(t *testing.T) {
	_, err := Call("get", []any{map[string]any{}, 42})
	if err == nil {
		t.Fatalf("expected error for non-string path")
	}
}

func TestSet_HappyPath(t *testing.T) {
	v, err := Call("set", []any{map[string]any{"a": 1}, "b", "added"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map", v)
	}
	if m["a"] != float64(1) || m["b"] != "added" {
		t.Errorf("got %v", m)
	}
}

func TestSet_NestedPath(t *testing.T) {
	v, err := Call("set", []any{map[string]any{}, "x.y.z", "deep"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ := Call("get", []any{v, "x.y.z"}); got != "deep" {
		t.Errorf("nested set/get: got %v", got)
	}
}

func TestSet_OverwriteExisting(t *testing.T) {
	v, err := Call("set", []any{map[string]any{"a": "old"}, "a", "new"})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	m := v.(map[string]any)
	if m["a"] != "new" {
		t.Errorf("got %v, want 'new'", m["a"])
	}
}

func TestHas(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		v, err := Call("has", []any{map[string]any{"x": "value"}, "x"})
		if err != nil {
			t.Fatalf("has: %v", err)
		}
		if v != true {
			t.Errorf("got %v, want true", v)
		}
	})
	t.Run("present-but-null", func(t *testing.T) {
		v, err := Call("has", []any{`{"x":null}`, "x"})
		if err != nil {
			t.Fatalf("has: %v", err)
		}
		if v != true {
			t.Errorf("present-but-null should still be true, got %v", v)
		}
	})
	t.Run("absent", func(t *testing.T) {
		v, err := Call("has", []any{map[string]any{"y": 1}, "x"})
		if err != nil {
			t.Fatalf("has: %v", err)
		}
		if v != false {
			t.Errorf("got %v, want false", v)
		}
	})
}

// --- constructors ------------------------------------------------

func TestObject_HappyPath(t *testing.T) {
	v, err := Call("object", []any{"a", int64(1), "b", "hello"})
	if err != nil {
		t.Fatalf("object: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map", v)
	}
	if m["a"] != int64(1) || m["b"] != "hello" {
		t.Errorf("got %v", m)
	}
}

func TestObject_Empty(t *testing.T) {
	v, err := Call("object", nil)
	if err != nil {
		t.Fatalf("object: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 0 {
		t.Errorf("got %T %v, want empty map", v, v)
	}
}

func TestObject_OddArgsErrors(t *testing.T) {
	_, err := Call("object", []any{"a", 1, "b"})
	if err == nil {
		t.Fatal("expected error for odd arg count")
	}
}

func TestObject_NonStringKeyErrors(t *testing.T) {
	_, err := Call("object", []any{42, "v"})
	if err == nil {
		t.Fatal("expected error for non-string key")
	}
}

func TestObject_DuplicateKeyLastWins(t *testing.T) {
	v, err := Call("object", []any{"k", "first", "k", "second"})
	if err != nil {
		t.Fatalf("object: %v", err)
	}
	m := v.(map[string]any)
	if m["k"] != "second" {
		t.Errorf("got %v, want 'second' (last-wins)", m["k"])
	}
}

func TestArray(t *testing.T) {
	v, err := Call("array", []any{int64(1), "x", true})
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	want := []any{int64(1), "x", true}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("got %v, want %v", v, want)
	}
}

func TestArray_Empty(t *testing.T) {
	v, err := Call("array", nil)
	if err != nil {
		t.Fatalf("array: %v", err)
	}
	a, ok := v.([]any)
	if !ok || len(a) != 0 {
		t.Errorf("got %T %v, want empty []any", v, v)
	}
}

// --- string / hash -----------------------------------------------

func TestConcat(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"strings", []any{"a", "b", "c"}, "abc"},
		{"with-int", []any{"v", int64(42)}, "v42"},
		{"with-nil", []any{"a", nil, "b"}, "ab"},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Call("concat", tc.args)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if v != tc.want {
				t.Errorf("got %q, want %q", v, tc.want)
			}
		})
	}
}

func TestLen(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"string", "hello", 5},
		{"array", []any{"x", "y"}, 2},
		{"map", map[string]any{"a": 1, "b": 2}, 2},
		{"empty-string", "", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Call("len", []any{tc.in})
			if err != nil {
				t.Fatalf("%v", err)
			}
			if v != tc.want {
				t.Errorf("got %v, want %v", v, tc.want)
			}
		})
	}
}

func TestLen_UnsupportedType(t *testing.T) {
	_, err := Call("len", []any{int64(42)})
	if err == nil {
		t.Fatal("expected error for int")
	}
}

func TestSplit(t *testing.T) {
	v, err := Call("split", []any{"a,b,c", ","})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	want := []any{"a", "b", "c"}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("got %v, want %v", v, want)
	}
}

func TestSplit_Empty(t *testing.T) {
	v, err := Call("split", []any{"abc", ""})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	want := []any{"a", "b", "c"}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("got %v, want %v", v, want)
	}
}

func TestJoin(t *testing.T) {
	v, err := Call("join", []any{[]any{"hello", "world", "thanks,"}, " "})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if v != "hello world thanks," {
		t.Errorf("got %q, want %q", v, "hello world thanks,")
	}
}

func TestJoin_CoercesAndNil(t *testing.T) {
	// non-strings coerce via Sprintf; nil → empty (matches &concat).
	v, err := Call("join", []any{[]any{"a", 2, nil, true}, "-"})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if v != "a-2--true" {
		t.Errorf("got %q, want %q", v, "a-2--true")
	}
}

func TestJoin_RejectsNonArray(t *testing.T) {
	if _, err := Call("join", []any{"not-an-array", " "}); err == nil {
		t.Errorf("join of a non-array should error")
	}
}

func TestSubstr(t *testing.T) {
	v, err := Call("substr", []any{"hello world", int64(0), int64(5)})
	if err != nil {
		t.Fatalf("substr: %v", err)
	}
	if v != "hello" {
		t.Errorf("got %q, want 'hello'", v)
	}
}

func TestSubstr_OutOfRange(t *testing.T) {
	_, err := Call("substr", []any{"hi", int64(0), int64(99)})
	if err == nil {
		t.Fatal("expected error for end > len")
	}
}

func TestSubstr_NegativeIndex(t *testing.T) {
	_, err := Call("substr", []any{"hi", int64(-1), int64(2)})
	if err == nil {
		t.Fatal("expected error for negative index")
	}
}

func TestSubstr_StartGreaterThanEnd(t *testing.T) {
	_, err := Call("substr", []any{"hello", int64(3), int64(1)})
	if err == nil {
		t.Fatal("expected error for start > end")
	}
}

func TestSubstr_AcceptsFloatIntegers(t *testing.T) {
	// JSON-numbered args arrive as float64 from json.Unmarshal.
	// substrFn must accept them as long as they're whole numbers.
	v, err := Call("substr", []any{"hello", float64(0), float64(3)})
	if err != nil {
		t.Fatalf("substr: %v", err)
	}
	if v != "hel" {
		t.Errorf("got %q", v)
	}
}

func TestSubstr_RejectsFractional(t *testing.T) {
	_, err := Call("substr", []any{"hello", float64(0.5), float64(3)})
	if err == nil {
		t.Fatal("expected error for fractional index")
	}
}

func TestRepeat(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"perl-x", []any{"hi ", int64(2)}, "hi hi "},
		{"rule", []any{"-", int64(10)}, "----------"},
		{"zero", []any{"x", int64(0)}, ""},
		{"float-count", []any{"ab", float64(3)}, "ababab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Call("repeat", tc.args)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if v != tc.want {
				t.Errorf("got %q, want %q", v, tc.want)
			}
		})
	}
}

func TestRepeat_NegativeCount(t *testing.T) {
	if _, err := Call("repeat", []any{"x", int64(-1)}); err == nil {
		t.Fatal("expected error for negative count")
	}
}

func TestRepeat_ExceedsCap(t *testing.T) {
	// 2 bytes × (cap) would blow past the 1 MiB cap → halt, not truncate.
	if _, err := Call("repeat", []any{"ab", int64(1 << 20)}); err == nil {
		t.Fatal("expected error for result exceeding the byte cap")
	}
}

func TestPad(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want string
	}{
		{"left-zero-pad", []any{"42", int64(5), "0"}, "00042"},
		{"right-pad", []any{"hi", int64(-5), " "}, "hi   "},
		{"already-wide", []any{"already", int64(3), "0"}, "already"},
		{"exact-width", []any{"abc", int64(3), "0"}, "abc"},
		{"multi-char-fill", []any{"7", int64(5), "ab"}, "abab7"}, // need 4 fill bytes → "abab"
		{"float-width", []any{"9", float64(3), "0"}, "009"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := Call("pad", tc.args)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if v != tc.want {
				t.Errorf("got %q, want %q", v, tc.want)
			}
		})
	}
}

func TestPad_ZeroWidth(t *testing.T) {
	if _, err := Call("pad", []any{"x", int64(0), "0"}); err == nil {
		t.Fatal("expected error for zero width")
	}
}

func TestPad_EmptyFillWhenPaddingNeeded(t *testing.T) {
	if _, err := Call("pad", []any{"42", int64(5), ""}); err == nil {
		t.Fatal("expected error for empty fill when padding is needed")
	}
}

func TestPad_EmptyFillPassthrough(t *testing.T) {
	// No padding needed → empty fill is harmless, s passes through.
	v, err := Call("pad", []any{"already", int64(3), ""})
	if err != nil {
		t.Fatalf("pad: %v", err)
	}
	if v != "already" {
		t.Errorf("got %q, want 'already'", v)
	}
}

func TestSha256(t *testing.T) {
	v, err := Call("sha256", []any{"hello"})
	if err != nil {
		t.Fatalf("sha256: %v", err)
	}
	// Known SHA-256 of "hello":
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if v != want {
		t.Errorf("got %q, want %q", v, want)
	}
}

// --- try_* variants ----------------------------------------------

func TestTryJson_BadInputReturnsNil(t *testing.T) {
	v, err := Call("try_json", []any{"not json"})
	if err != nil {
		t.Fatalf("try_json should not error: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestTryJson_GoodInputPassesThrough(t *testing.T) {
	v, err := Call("try_json", []any{`{"a":1}`})
	if err != nil {
		t.Fatalf("%v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["a"] != float64(1) {
		t.Errorf("got %v", v)
	}
}

func TestTryB64decode_BadInputReturnsNil(t *testing.T) {
	v, err := Call("try_b64decode", []any{"not!!base64"})
	if err != nil {
		t.Fatalf("try_b64decode should not error: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestTryUrldecode_BadInputReturnsNil(t *testing.T) {
	v, err := Call("try_urldecode", []any{"%zz"})
	if err != nil {
		t.Fatalf("try_urldecode should not error: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestTryGet_UnwalkableReturnsNil(t *testing.T) {
	// Strict &get errors on non-JSON-string obj; try_get swallows.
	v, err := Call("try_get", []any{"hello", "x"})
	if err != nil {
		t.Fatalf("try_get should not error: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestTryGet_MissingPathReturnsNil(t *testing.T) {
	// Strict &get already returns nil on missing path — try_get
	// should behave identically.
	v, err := Call("try_get", []any{map[string]any{"x": 1}, "y"})
	if err != nil {
		t.Fatalf("try_get: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}

func TestTrySubstr_OutOfRangeReturnsNil(t *testing.T) {
	v, err := Call("try_substr", []any{"hi", int64(0), int64(99)})
	if err != nil {
		t.Fatalf("try_substr should not error: %v", err)
	}
	if v != nil {
		t.Errorf("got %v, want nil", v)
	}
}
