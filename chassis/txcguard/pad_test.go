package txcguard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestPadsArray(t *testing.T) {
	cases := []struct {
		doc, path string
		want      bool
	}{
		// the everyday paths: never refused
		{`{}`, "london.time", false},
		{`{}`, "_txc.web.res.headers.content-type.0", false},
		{`{}`, "items.-1", false}, // append
		{`{}`, "items.12", false}, // a small sparse array is fine
		{`{}`, "items.65535", false},

		// a numeric key under a missing / scalar / short-array parent pads
		{`{}`, "a.2000000", true},
		{`{}`, "a.65536", true},
		{`{"a":1}`, "a.2000000", true},
		{`{"a":[1,2]}`, "a.2000000", true},
		{`{}`, "a.b.c.2000000.d", true},
		{`{"a":{}}`, "a.b.2000000", true}, // `b` is missing, so it becomes an array
		{``, "2000000", true},             // no document: the root itself becomes an array
		{`[]`, "2000000", true},

		// …in every spelling sjson reads as that index
		{`{}`, `a.\2000000`, true},
		{`{}`, `a.\2\0\0\0\0\0\0`, true},
		{`{}`, "a.0002000000", true},
		{`{}`, "a.99999999999999999999999", true}, // overflows int: unbounded

		// the padding is summed along the path
		{`{}`, "a.40000.b.40000", true},
		{`{}`, "a.30000.b.30000", false},

		// the same keys where sjson does NOT build an array are left alone
		{`{"by_id":{}}`, "by_id.1690000000", false},      // object parent → object key
		{`{"by_id":{}}`, "by_id.1690000000.name", false}, // and below it
		{`{}`, "1690000000", false},                      // first key on the object root
		{`{}`, "by_id.:1690000000", false},               // ':' forces an object key
		{`{}`, "when.2026-09-18", false},                 // not all digits
		{`{"a":[0,1,2,3]}`, "a.2", false},                // replaces in place

		// extending an existing array counts only the new elements
		{`{"a":` + bigArray(70000) + `}`, "a.70001", false},
		{`{"a":` + bigArray(10) + `}`, "a.70001", true},

		// a path sjson rejects writes nothing
		{`{}`, "a.*.2000000", false},
	}
	for _, c := range cases {
		doc := c.doc
		if len(doc) > 40 {
			doc = doc[:40] + "…"
		}
		if got := PadsArray(c.doc, c.path); got != c.want {
			t.Errorf("PadsArray(%s, %q) = %v, want %v", doc, c.path, got, c.want)
		}
	}
}

func bigArray(n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat("0,", n), ",") + "]"
}

func TestBoundedSet(t *testing.T) {
	const doc = `{"a":1}`
	for _, path := range []string{"x.2000000", `x.\2000000`, "x.99999999999999999999999"} {
		out, err := BoundedSet(doc, path, 1)
		if !errors.Is(err, ErrArrayPad) || out != doc {
			t.Errorf("BoundedSet(%q) = %.40q, %v; want the doc unchanged and ErrArrayPad", path, out, err)
		}
		out, err = BoundedSetRaw(doc, path, `{"k":1}`)
		if !errors.Is(err, ErrArrayPad) || out != doc {
			t.Errorf("BoundedSetRaw(%q) = %.40q, %v; want the doc unchanged and ErrArrayPad", path, out, err)
		}
		outB, err := BoundedSetBytes([]byte(doc), path, 1)
		if !errors.Is(err, ErrArrayPad) || string(outB) != doc {
			t.Errorf("BoundedSetBytes(%q) = %.40q, %v; want the doc unchanged and ErrArrayPad", path, outB, err)
		}
	}

	// Otherwise it IS sjson.
	for _, path := range []string{"b", "c.d.0", "c.-1", "by_id.:1690000000", "a"} {
		want, werr := sjson.Set(doc, path, "v")
		got, gerr := BoundedSet(doc, path, "v")
		if got != want || (gerr == nil) != (werr == nil) {
			t.Errorf("BoundedSet(%q) = %q, %v; sjson.Set = %q, %v", path, got, gerr, want, werr)
		}
	}
	if got, _ := BoundedSet(`{"by_id":{}}`, "by_id.1690000000", "u"); gjson.Get(got, `by_id.1690000000`).String() != "u" {
		t.Errorf("a numeric OBJECT key was refused or misplaced: %s", got)
	}
}

// A reserved-path check is not enough for an op target: the target also sizes
// the write.
func TestTargetsRefuseArrayPad(t *testing.T) {
	for _, raw := range []string{".rows.2000000", "rows.2000000.x", `rows.\2000000`, "@web.res.headers.x.2000000"} {
		if _, ok := AuthorTarget(raw); ok {
			t.Errorf("AuthorTarget(%q) allowed a padding target", raw)
		}
		if _, ok := ComputedTarget(raw); ok {
			t.Errorf("ComputedTarget(%q) allowed a padding target", raw)
		}
	}
	for _, raw := range []string{".rows.0", "rows.-1", "rows.:2000000", "@web.res.headers.content-type.0"} {
		if _, ok := AuthorTarget(raw); !ok {
			t.Errorf("AuthorTarget(%q) refused an ordinary target", raw)
		}
	}
}

func TestPlainPathPadCheckDoesNotAllocate(t *testing.T) {
	const doc = `{"london":{"time":"12:00"}}`
	if n := testing.AllocsPerRun(100, func() { PadsArray(doc, "london.items.12") }); n != 0 {
		t.Errorf("PadsArray on an everyday path allocates %v times per call, want 0", n)
	}
}

// countNulls counts `null` VALUES (array padding is exactly that).
func countNulls(v any) int {
	switch x := v.(type) {
	case nil:
		return 1
	case []any:
		n := 0
		for _, e := range x {
			n += countNulls(e)
		}
		return n
	case map[string]any:
		n := 0
		for _, e := range x {
			n += countNulls(e)
		}
		return n
	}
	return 0
}

var padDocs = []string{
	`{}`, ``, `[]`, `[1,2,3]`, `{"a":{}}`, `{"a":1}`, `{"a":[1,2]}`,
	`{"a":[[1],{"b":[]}]}`, `{"a":{"5":[1]},"5":{"a":[]}}`, `{"a":{"b":{"c":[0,0,0,0,0,0]}}}`,
}

// checkPadAgainstSjson is the property, tested against the real writer with
// a small limit so a miss is cheap: whenever padsArray ALLOWS a write, sjson
// performing it adds at most `max` null elements.
func checkPadAgainstSjson(t *testing.T, doc, path string) {
	t.Helper()
	const max = 8
	if padsArray(doc, path, max) {
		return
	}
	out, err := sjson.Set(doc, path, 1)
	if err != nil || out == "" {
		return
	}
	var before, after any
	if doc != "" {
		if json.Unmarshal([]byte(doc), &before) != nil {
			return
		}
	}
	if err := json.Unmarshal([]byte(out), &after); err != nil {
		return // sjson produced non-JSON for an odd path; not a padding question
	}
	base := 0
	if doc != "" {
		base = countNulls(before)
	}
	if added := countNulls(after) - base; added > max {
		t.Errorf("padsArray(%q, %q, %d) allowed a write that added %d nulls: %.200s", doc, path, max, added, out)
	}
}

var padSeeds = []string{
	"a.9", "a.8", "a.12", "a.5.5", "a.4.4.4", `a.\9`, "a.:9", "a.09", "a.-1", "5.a.9", "9", "a.0.9",
	"a.1.b.9", "a.b.c.7", "a.b.c.20", `a.\1\2`, "a.b.9.9", "a..9", "a.9.", `a\.9`, "a.5.0.9",
}

func TestPadAgainstSjson(t *testing.T) {
	for _, doc := range padDocs {
		for _, p := range padSeeds {
			checkPadAgainstSjson(t, doc, p)
		}
	}
}

func FuzzPadAgainstSjson(f *testing.F) {
	for i := range padDocs {
		for _, p := range padSeeds {
			f.Add(uint8(i), p)
		}
	}
	f.Fuzz(func(t *testing.T, docIdx uint8, path string) {
		if hasBigIndex(path) {
			t.Skip() // keep a miss cheap: indexes stay under 1000
		}
		checkPadAgainstSjson(t, padDocs[int(docIdx)%len(padDocs)], path)
	})
}
