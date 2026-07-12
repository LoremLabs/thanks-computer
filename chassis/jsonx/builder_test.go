package jsonx

import (
	"math"
	"math/rand"
	"testing"

	"github.com/tidwall/sjson"
)

// The Builder's one job is byte-identity with a chain of
// `doc, _ = sjson.Set(doc, path, v)` calls. These tests hold it to
// that across targeted goldens, 20k random op sequences, and a fuzz
// tape (builder_fuzz_test.go).

type bop struct {
	raw  bool
	path string
	val  any
	rawV string
}

func applyChain(start int, ops []bop) string {
	doc := ""
	switch start {
	case 1:
		doc = "{}"
	case 2:
		doc = "[]"
	}
	for _, op := range ops {
		if op.raw {
			doc, _ = sjson.SetRaw(doc, op.path, op.rawV)
		} else {
			doc, _ = sjson.Set(doc, op.path, op.val)
		}
	}
	return doc
}

func applyBuilder(start int, ops []bop) string {
	var b *Builder
	switch start {
	case 1:
		b = NewObject()
	case 2:
		b = NewArray()
	default:
		b = New()
	}
	for _, op := range ops {
		if op.raw {
			b.SetRaw(op.path, op.rawV)
		} else {
			b.Set(op.path, op.val)
		}
	}
	return b.String()
}

func assertOps(t *testing.T, start int, ops []bop) {
	t.Helper()
	want := applyChain(start, ops)
	got := applyBuilder(start, ops)
	if got != want {
		t.Fatalf("mismatch (start=%d)\nops:  %+v\nsjson: %q\njsonx: %q", start, ops, want, got)
	}
}

func TestBuilderGoldens(t *testing.T) {
	cases := map[string][]bop{
		"prepend-order": {
			{path: "a", val: 1}, {path: "b", val: 2}, {path: "c", val: 3},
		},
		"replace-in-place": {
			{path: "a", val: 1}, {path: "b", val: 2}, {path: "a", val: 9},
		},
		"nested-create": {
			{path: "x.y.z", val: "deep"}, {path: "x.q", val: true},
		},
		"array-append": {
			{path: "l.-1", val: 1}, {path: "l.-1", val: 2}, {path: "l.-1.k", val: 3},
		},
		"numeric-pad": {
			{path: "l.-1", val: "first"}, {path: "l.4", val: "fifth"},
		},
		"numeric-root": {
			{path: "2", val: "third"},
		},
		"neg-one-root-is-key": {
			{path: "-1", val: "keyed"},
		},
		"forced-string-key": {
			{path: "o.:5", val: "keyed"}, {path: "o.:-1", val: "also"},
		},
		"escaped-dot": {
			{path: `a\.b`, val: 1}, {path: `a\.b`, val: 2},
		},
		"setraw-then-descend": {
			{raw: true, path: "o", rawV: `{"x":1}`}, {path: "o.y", val: 2},
		},
		"setraw-scalar-descend": {
			{raw: true, path: "o", rawV: `5`}, {path: "o.y", val: 2},
		},
		"poison-and-rebuild": {
			{path: "a", val: 1}, {path: "bad*path", val: 2}, {path: "c", val: 3},
		},
		"array-nonnumeric-poisons": {
			{path: "0", val: 1}, {path: "key", val: 2}, {path: "again", val: 3},
		},
		"float32-quirk": {
			{path: "f", val: float32(1.1)},
		},
		"marshal-default": {
			{path: "m", val: map[string]any{"z": 1, "a": "<b>"}},
		},
		"unicode-string": {
			{path: "s", val: "uni é🎈 <&>"},
		},
	}
	for name, ops := range cases {
		t.Run(name, func(t *testing.T) {
			assertOps(t, 0, ops)
			assertOps(t, 1, ops)
			assertOps(t, 2, ops)
		})
	}
}

var builderPathPool = []string{
	"a", "b", "Key", "key", "_txc", "0", "1", "007", "-1", ":-1",
	"user name", "emoji\U0001F388", `a\.b`, "<tag>",
	"a.b", "a.0", "a.-1", "a.:-1", "_txc.web.req.host", "arr.5.x",
	"a.b.c.d", "0.x", "list.-1.name", "k.007.z", "a..b",
	// poison paths
	"wild*", "q?x", "arr#", "",
}

var builderValPool = []any{
	"plain", `esc"ape`, "uni é\U0001F388", "<amp>&", "",
	int(42), int64(-7), uint8(255), int32(1 << 20),
	float64(1e21), float64(0.5), float32(1.1), math.Copysign(0, -1),
	true, false, nil,
	map[string]any{"z": 1, "a": "b"}, []any{1, "two", nil},
	[]byte("bytes<>"),
	struct {
		A int    `json:"a"`
		B string `json:"b"`
	}{7, "s"},
}

var builderRawPool = []string{
	"1", "true", "null", `"s"`, `{"x":1}`, `[1,2]`,
	`{"n":{"m":2}}`, "-0.5", `{"deep":{"list":[{"a":1}]}}`,
}

func randOps(r *rand.Rand) []bop {
	n := 1 + r.Intn(10)
	ops := make([]bop, 0, n)
	for i := 0; i < n; i++ {
		if r.Intn(4) == 0 {
			ops = append(ops, bop{
				raw:  true,
				path: builderPathPool[r.Intn(len(builderPathPool))],
				rawV: builderRawPool[r.Intn(len(builderRawPool))],
			})
		} else {
			ops = append(ops, bop{
				path: builderPathPool[r.Intn(len(builderPathPool))],
				val:  builderValPool[r.Intn(len(builderValPool))],
			})
		}
	}
	return ops
}

func TestBuilderMatchesSjsonChain(t *testing.T) {
	r := rand.New(rand.NewSource(20260712))
	for i := 0; i < 20000; i++ {
		ops := randOps(r)
		start := r.Intn(3)
		want := applyChain(start, ops)
		got := applyBuilder(start, ops)
		if got != want {
			t.Fatalf("iter %d mismatch (start=%d)\nops:  %+v\nsjson: %q\njsonx: %q",
				i, start, ops, want, got)
		}
	}
}
