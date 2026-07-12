package jsonx

import (
	"math/rand"
	"testing"

	"github.com/tidwall/sjson"
)

func setManyChain(doc string, sets []PathVal) string {
	for _, s := range sets {
		doc, _ = sjson.Set(doc, s.Path, s.Val)
	}
	return doc
}

func TestSetManyGoldens(t *testing.T) {
	cases := []struct {
		doc  string
		sets []PathVal
	}{
		{`{"_txc":{"fuel_used":10,"ttl":5,"_seen":["a"]}}`, []PathVal{
			{"_txc.fuel_used", 12}, {"_txc.ttl", 4},
		}},
		{`{"a":1}`, []PathVal{{"a", 2}, {"b", 3}}},                       // b missing → chain
		{`{"a":{"b":1}}`, []PathVal{{"a", 5}, {"a.b", 6}}},              // overlap → chain
		{`{"a":"x"}`, []PathVal{{"a", "uni é🎈"}}},                       // re-encode
		{`{"a":1,"a":2}`, []PathVal{{"a", 9}}},                          // dup keys
		{`{ "a": 1 }`, []PathVal{{"a", 2}}},                             // whitespace doc
		{`{"l":[1,2,3]}`, []PathVal{{"l.1", 9}}},                        // array index
		{`{"l":[1,2,3]}`, []PathVal{{"l.7", 9}}},                        // out of range → chain
		{`{"a":1}`, []PathVal{{"bad*", 2}}},                             // poison path → chain
		{`{"a":1}`, nil},                                                // no sets
		{`{"a":true,"b":"s","c":null}`, []PathVal{{"a", false}, {"b", "t"}, {"c", 1.5}}},
	}
	for _, c := range cases {
		want := setManyChain(c.doc, c.sets)
		got := SetMany(c.doc, c.sets)
		if got != want {
			t.Fatalf("SetMany mismatch\ndoc:  %q\nsets: %+v\nwant %q\ngot  %q", c.doc, c.sets, want, got)
		}
	}
}

func TestSetManyMatchesChain(t *testing.T) {
	r := rand.New(rand.NewSource(20260712))
	paths := []string{"a", "b", "a.b", "a.c", "_txc.fuel_used", "_txc.ttl", "l.0", "l.-1", "l.5", "missing.x", "a..b", "Key"}
	docs := []string{
		`{}`,
		`{"a":{"b":1,"c":"s"},"b":2,"_txc":{"fuel_used":10,"ttl":3},"l":[1,2],"Key":null}`,
		`{"a":1,"l":[]}`,
		`{ "a": {"b": 2}, "_txc": {"fuel_used": 1, "ttl": 2} }`,
	}
	for i := 0; i < 20000; i++ {
		doc := docs[r.Intn(len(docs))]
		n := r.Intn(5)
		sets := make([]PathVal, 0, n)
		for j := 0; j < n; j++ {
			sets = append(sets, PathVal{
				Path: paths[r.Intn(len(paths))],
				Val:  builderValPool[r.Intn(len(builderValPool))],
			})
		}
		want := setManyChain(doc, sets)
		got := SetMany(doc, sets)
		if got != want {
			t.Fatalf("iter %d SetMany mismatch\ndoc:  %q\nsets: %+v\nwant %q\ngot  %q", i, doc, sets, want, got)
		}
	}
}
