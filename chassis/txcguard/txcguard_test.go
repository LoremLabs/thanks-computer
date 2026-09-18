package txcguard

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tidwall/sjson"
)

func TestAuthorMayWrite(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// the author's own data
		{"london.time", true},
		{"_files.welcome", true},
		{"a:b.c", true},             // ':' is only special at the start of a segment
		{`users.alice\.test`, true}, // an escaped dot in an author key
		{"_txcx.tenant", true},      // lookalike root key — not the control plane
		{" _txc.tenant", true},      // leading space → the literal key " _txc"
		{`_txc\.tenant`, true},      // escaped dot → the root key "_txc.tenant"

		// author-writable control paths
		{"_txc.web.res", true},
		{"_txc.web.res.body", true},
		{"_txc.goto", true},
		{"_txc.halt", true},
		{"_txc.telemetry.metrics", true},
		{"_txc.llm.context.0", true},

		// reserved
		{"_txc", false},
		{"_txc.tenant", false},
		{"_txc.fuel_used", false},
		{"_txc.ttl", false},
		{"_txc._seen", false},
		{"_txc.route.to", false},
		{"_txc.computed.sig_valid", false},
		{"_txc.imap.account", false},
		{"_txc.principal", false},
		{"_txc.web", false}, // parent of an allowed subtree
		{"_txc.web.req.url", false},
		{"_txc.web.resx", false}, // lookalike leaf
		{"_txc.gotox", false},
		{"_txc..tenant", false},

		// the same reserved paths in spellings sjson resolves identically:
		// an escape is dropped (`\x` → `x`) and so is a segment's leading ':'.
		// A prefix test on the raw string let every one of these through.
		{`\_txc.tenant`, false},
		{`_tx\c.tenant`, false},
		{`_txc.ten\ant`, false},
		{":_txc.tenant", false},
		{"_txc.:tenant", false},
		{`:\_txc.fuel_used`, false},
		{`_txc.web\.res.x`, false}, // key "web.res" under _txc — not web → res
		{`_txc.web.res\.x`, false}, // key "res.x" under _txc.web

		// …and the allowed ones stay allowed however they are spelled
		{`:_txc.web.res.body`, true},
		{`_txc.we\b.res.status`, true},

		// paths sjson rejects fail closed (nothing would be written)
		{"", false},
		{"_txc.*", false},
		{"_t?c.tenant", false},
		{"items.#.x", false},
	}
	for _, c := range cases {
		if got := AuthorMayWrite(c.path); got != c.want {
			t.Errorf("AuthorMayWrite(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestAuthorMayDelete(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"london", true},
		{"_txc.web.res.body", true},  // writable ⇒ deletable
		{"_txc.web.req.body", true},  // delete-only inbound fact
		{`:_txc.web.req.body`, true}, // however it is spelled
		{"_txc.imap.msg.headers.x", true},
		{"_txc.web.req.url", false},
		{"_txc.tenant", false},
		{"_txc.fuel_used", false},
		{"_txc._seen", false},
		{`\_txc.fuel_used`, false},
		{":_txc._seen", false},
		{"_txc", false},
	}
	for _, c := range cases {
		if got := AuthorMayDelete(c.path); got != c.want {
			t.Errorf("AuthorMayDelete(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestSystemMayWrite(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"_txc.route", true},
		{"_txc.route.tenant", true},
		{":_txc.route.to", true},
		{"_txc.routes", false},
		{"_txc.route_x", false},
		{"_txc.tenant", false},
		{"route.to", false}, // outside _txc: not the system set (it is plain author data)
		{"_txc", false},
	}
	for _, c := range cases {
		if got := SystemMayWrite(c.path); got != c.want {
			t.Errorf("SystemMayWrite(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestTargets(t *testing.T) {
	cases := []struct {
		raw          string
		wantPath     string
		wantAuthor   bool
		wantComputed bool
	}{
		{"", "", true, true}, // not given — the caller defaults
		{".files", "files", true, true},
		{"_kv", "_kv", true, true},
		{"@web.res.body", "_txc.web.res.body", true, true},
		{"_txc.web.res.body", "_txc.web.res.body", true, true},
		// a verdict path: fine for an auth helper, not for author data
		{"_txc.computed.sig_valid", "_txc.computed.sig_valid", false, true},
		{"@computed.stripe_sig", "_txc.computed.stripe_sig", false, true},
		// reserved for both
		{"@tenant", "_txc.tenant", false, false},
		{"@imap.account", "_txc.imap.account", false, false},
		{"@principal", "_txc.principal", false, false},
		{`\_txc.tenant`, `\_txc.tenant`, false, false},
		{":_txc.tenant", ":_txc.tenant", false, false},
		{"_txc", "_txc", false, false},
		{"_txc.computedx.a", "_txc.computedx.a", false, false},
		// a dangling escape would swallow the '.' a handler appends
		{`_txc.web.res\`, `_txc.web.res\`, false, false},
		{`mine\`, `mine\`, false, false},
		{`mine\\`, `mine\\`, true, true}, // a paired (literal) backslash is fine
	}
	for _, c := range cases {
		p, ok := AuthorTarget(c.raw)
		if p != c.wantPath || ok != c.wantAuthor {
			t.Errorf("AuthorTarget(%q) = %q, %v; want %q, %v", c.raw, p, ok, c.wantPath, c.wantAuthor)
		}
		p, ok = ComputedTarget(c.raw)
		if p != c.wantPath || ok != c.wantComputed {
			t.Errorf("ComputedTarget(%q) = %q, %v; want %q, %v", c.raw, p, ok, c.wantPath, c.wantComputed)
		}
	}
}

func TestAuthorWritablePathsIsACopy(t *testing.T) {
	got := AuthorWritablePaths()
	got[0] = "tenant"
	if AuthorMayWrite("_txc.tenant") {
		t.Fatal("mutating the returned slice widened the policy")
	}
}

// guardDoc carries one value in every class of `_txc` path: reserved
// (tenant, fuel_used, route, computed, web.req), delete-only (web.req.body),
// author-writable (web.res), plus author data.
const guardDoc = `{"_txc":{"tenant":"real","fuel_used":7,"_seen":["a"],"route":{"to":"x"},` +
	`"computed":{"ok":false},"web":{"req":{"url":"u","body":"b"},"res":{"status":200}}},"mine":{"k":1}}`

// reservedRemainder is what is left of doc's `_txc` once the subtrees in
// `allowed` are removed — i.e. everything the author must not have changed.
// Emptied objects are pruned so "added then removed an allowed subtree" and
// "never had it" compare equal.
func reservedRemainder(t *testing.T, doc string, allowed ...[]string) any {
	t.Helper()
	for _, list := range allowed {
		for _, entry := range list {
			if out, err := sjson.Delete(doc, "_txc."+entry); err == nil {
				doc = out
			}
		}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("result is not a JSON object: %v: %s", err, doc)
	}
	return prune(v["_txc"])
}

func prune(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for k, c := range m {
		c = prune(c)
		if cm, isMap := c.(map[string]any); isMap && len(cm) == 0 {
			delete(m, k)
			continue
		}
		m[k] = c
	}
	return m
}

// checkGuardAgainstSjson is the property the whole package exists for, tested
// against the real writer rather than our model of it: whatever path the
// guard ALLOWS, sjson applying it leaves every reserved `_txc` value exactly
// as it was.
func checkGuardAgainstSjson(t *testing.T, path string) {
	t.Helper()
	wantWrite := reservedRemainder(t, guardDoc, authorWritable)
	if AuthorMayWrite(path) {
		if out, err := sjson.Set(guardDoc, path, "FORGED"); err == nil {
			if got := reservedRemainder(t, out, authorWritable); !reflect.DeepEqual(got, wantWrite) {
				t.Errorf("AuthorMayWrite(%q) allowed a write that changed reserved _txc:\n got %v\nwant %v", path, got, wantWrite)
			}
		}
	}
	if p, ok := ComputedTarget(path); ok && p != "" {
		want := reservedRemainder(t, guardDoc, authorWritable, computedWritable)
		// A handler appends to its target, so check the target and a child.
		for _, full := range []string{p, p + ".items"} {
			if out, err := sjson.Set(guardDoc, full, "FORGED"); err == nil {
				if got := reservedRemainder(t, out, authorWritable, computedWritable); !reflect.DeepEqual(got, want) {
					t.Errorf("ComputedTarget(%q) allowed %q, which changed reserved _txc:\n got %v\nwant %v", path, full, got, want)
				}
			}
		}
	}
	if p, ok := AuthorTarget(path); ok && p != "" {
		for _, full := range []string{p, p + ".items", p + ".error.code"} {
			if out, err := sjson.Set(guardDoc, full, "FORGED"); err == nil {
				if got := reservedRemainder(t, out, authorWritable); !reflect.DeepEqual(got, wantWrite) {
					t.Errorf("AuthorTarget(%q) allowed %q, which changed reserved _txc:\n got %v\nwant %v", path, full, got, wantWrite)
				}
			}
		}
	}
	if AuthorMayDelete(path) {
		want := reservedRemainder(t, guardDoc, authorWritable, authorDeletable)
		if out, err := sjson.Delete(guardDoc, path); err == nil {
			if got := reservedRemainder(t, out, authorWritable, authorDeletable); !reflect.DeepEqual(got, want) {
				t.Errorf("AuthorMayDelete(%q) allowed a delete that changed reserved _txc:\n got %v\nwant %v", path, got, want)
			}
		}
	}
}

var guardSeeds = []string{
	"mine.k", "_txc", "_txc.tenant", "@tenant", "_txc.web.res.status", "_txc.web.req.body",
	`\_txc.tenant`, `_tx\c.tenant`, ":_txc.tenant", "_txc.:tenant", `:\_txc.fuel_used`,
	`_txc.web.res\`, `_txc.web\.res.x`, `_txc\.tenant`, " _txc.tenant", "_txc..tenant",
	"_txc.computed.ok", "@computed.ok", `_txc.computed\`, "_txc.route.to", "_txc._seen.0",
	"_txc._seen.-1", "_txc.web.req", `\:_txc.tenant`, `::_txc.tenant`, `_txc\\.tenant`,
}

func TestGuardAgainstSjson(t *testing.T) {
	for _, p := range guardSeeds {
		checkGuardAgainstSjson(t, p)
	}
}

func FuzzGuardAgainstSjson(f *testing.F) {
	for _, p := range guardSeeds {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, path string) {
		if hasBigIndex(path) {
			t.Skip() // sjson pads an array out to a numeric key: `a.2000000` is a 10 MB write
		}
		checkGuardAgainstSjson(t, path)
	})
}

// hasBigIndex reports a run of 3+ digits anywhere in path — a cheap
// over-approximation of "some key is a large array index".
func hasBigIndex(path string) bool {
	run := 0
	for i := 0; i < len(path); i++ {
		if path[i] >= '0' && path[i] <= '9' {
			if run++; run >= 3 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// The guard runs per EMIT/SET override on every op, so the common case — a
// plain author key — must stay allocation-free.
func BenchmarkAuthorMayWrite(b *testing.B) {
	for _, bc := range []struct{ name, path string }{
		{"author_key", "london.time"},
		{"txc_allowed", "_txc.web.res.body"},
		{"txc_reserved", "_txc.tenant"},
		{"escaped", `\_txc.tenant`},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				AuthorMayWrite(bc.path)
			}
		})
	}
}

func TestAuthorKeyCheckDoesNotAllocate(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() { AuthorMayWrite("london.time") }); n != 0 {
		t.Errorf("AuthorMayWrite on a plain author key allocates %v times per call, want 0", n)
	}
}
