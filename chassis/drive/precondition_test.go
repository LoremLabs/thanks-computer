package drive

import "testing"

// The conditional headers are the only real protection on a fleet whose
// locks are a stateless shim, so the parse has to hold up against what RFC
// 7232 actually allows — not just the single bare etag every client we run
// today happens to send.
func TestParseETagList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []etagEntry
	}{
		{"", nil},
		{`   `, nil},
		{"*", []etagEntry{{tag: "*"}}},
		{"abc", []etagEntry{{tag: "abc"}}},   // bare: how a stack op names one
		{`"abc"`, []etagEntry{{tag: "abc"}}}, // quoted: how a client sends one
		{`W/"abc"`, []etagEntry{{tag: "abc", weak: true}}},
		{`"a", "b"`, []etagEntry{{tag: "a"}, {tag: "b"}}},
		{`"a","b"`, []etagEntry{{tag: "a"}, {tag: "b"}}},
		{`"a", W/"b" , "c"`, []etagEntry{{tag: "a"}, {tag: "b", weak: true}, {tag: "c"}}},
		// A comma INSIDE the quotes belongs to the etag; splitting on
		// commas would invent two entries that match nothing.
		{`"a,b"`, []etagEntry{{tag: "a,b"}}},
		{`"a,b", "c"`, []etagEntry{{tag: "a,b"}, {tag: "c"}}},
		// Malformed: keep what parsed rather than inventing an entry.
		{`"a", "unterminated`, []etagEntry{{tag: "a"}}},
	} {
		got := parseETagList(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q entry %d: got %+v, want %+v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestCheckPreconditions(t *testing.T) {
	const live, gone = true, false
	for _, tc := range []struct {
		name                 string
		ifMatch, ifNoneMatch string
		live                 bool
		etag                 string
		wantErr              bool
	}{
		{name: "no headers", live: live, etag: "a"},

		{name: "if-match hit", ifMatch: `"a"`, live: live, etag: "a"},
		{name: "if-match miss", ifMatch: `"b"`, live: live, etag: "a", wantErr: true},
		{name: "if-match on a missing resource", ifMatch: `"a"`, live: gone, wantErr: true},
		{name: "if-match wildcard on a live one", ifMatch: "*", live: live, etag: "a"},
		{name: "if-match wildcard on a missing one", ifMatch: "*", live: gone, wantErr: true},
		// The list: ANY entry satisfies it. Before the list parse, this
		// trimmed to `a", "b` and failed closed — a spurious 412.
		{name: "if-match list, first hit", ifMatch: `"a", "b"`, live: live, etag: "a"},
		{name: "if-match list, last hit", ifMatch: `"a", "b"`, live: live, etag: "b"},
		{name: "if-match list, no hit", ifMatch: `"a", "b"`, live: live, etag: "c", wantErr: true},
		// If-Match compares STRONGLY: a weak entry is never enough.
		{name: "if-match weak", ifMatch: `W/"a"`, live: live, etag: "a", wantErr: true},
		{name: "if-match list with a weak hit only", ifMatch: `"b", W/"a"`, live: live, etag: "a", wantErr: true},

		{name: "if-none-match hit", ifNoneMatch: `"a"`, live: live, etag: "a", wantErr: true},
		{name: "if-none-match miss", ifNoneMatch: `"b"`, live: live, etag: "a"},
		{name: "if-none-match on a missing resource", ifNoneMatch: `"a"`, live: gone},
		{name: "if-none-match wildcard, create-only", ifNoneMatch: "*", live: live, etag: "a", wantErr: true},
		{name: "if-none-match wildcard on a missing one", ifNoneMatch: "*", live: gone},
		// The dangerous direction: before the list parse this trimmed to
		// `a", "b`, matched nothing, and the guard silently PASSED.
		{name: "if-none-match list, first hit", ifNoneMatch: `"a", "b"`, live: live, etag: "a", wantErr: true},
		{name: "if-none-match list, last hit", ifNoneMatch: `"a", "b"`, live: live, etag: "b", wantErr: true},
		{name: "if-none-match list, no hit", ifNoneMatch: `"a", "b"`, live: live, etag: "c"},
		// If-None-Match compares weakly: the prefix is noise.
		{name: "if-none-match weak hit", ifNoneMatch: `W/"a"`, live: live, etag: "a", wantErr: true},

		{name: "both, both satisfied", ifMatch: `"a"`, ifNoneMatch: `"b"`, live: live, etag: "a"},
		{name: "both, if-none-match refuses", ifMatch: `"a"`, ifNoneMatch: `"a"`, live: live, etag: "a", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPreconditions(tc.ifMatch, tc.ifNoneMatch, tc.live, tc.etag)
			if tc.wantErr && err == nil {
				t.Fatal("wanted a precondition failure, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("wanted no failure, got %v", err)
			}
		})
	}
}
