package storeseed

import (
	"bytes"
	"testing"
)

func TestKindForPath(t *testing.T) {
	cases := map[string]string{
		"VECTORS/books.jsonl":  KindVector,
		"VECTORS/a/b.jsonl":    KindVector, // prefix match (PackName rejects nesting separately)
		"KV/config.jsonl":      KindKV,
		"FILES/index.html":     "",
		"100/route.txcl":       "",
		"vectors/books.jsonl":  "", // case-sensitive
		"":                     "",
		"VECTORSX/books.jsonl": "", // prefix must be the dir + slash
		"VECTORS":              "", // bare dir
	}
	for p, want := range cases {
		if got := KindForPath(p); got != want {
			t.Errorf("KindForPath(%q) = %q, want %q", p, got, want)
		}
		if got := IsPackPath(p); got != (want != "") {
			t.Errorf("IsPackPath(%q) = %v, want %v", p, got, want != "")
		}
	}
}

func TestPackName(t *testing.T) {
	cases := map[string]string{
		"VECTORS/books.jsonl":    "books",
		"KV/seed-config.jsonl":   "seed-config",
		"VECTORS/nested/x.jsonl": "", // no nesting
		"VECTORS/books.json":     "", // wrong ext
		"VECTORS/.jsonl":         "", // empty-ish name still has a stem "" → reject
		"FILES/index.html":       "",
	}
	for p, want := range cases {
		if got := PackName(p); got != want {
			t.Errorf("PackName(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestNewRawPackAndLines(t *testing.T) {
	body := []byte(`{"id":"a","vector":[0.1,0.2]}

{"id":"b","vector":[0.3,0.4]}
`)
	p, ok := NewRawPack("VECTORS/books.jsonl", body)
	if !ok {
		t.Fatal("NewRawPack(VECTORS/books.jsonl) ok=false, want true")
	}
	if p.Kind != KindVector || p.Name != "books" {
		t.Fatalf("RawPack = {Kind:%q Name:%q}, want {vector books}", p.Kind, p.Name)
	}
	lines := p.Lines()
	if len(lines) != 2 {
		t.Fatalf("Lines() = %d lines, want 2 (blank line skipped)", len(lines))
	}
	if !bytes.Contains(lines[0], []byte(`"id":"a"`)) || !bytes.Contains(lines[1], []byte(`"id":"b"`)) {
		t.Errorf("Lines() = %q, want a then b", lines)
	}

	if _, ok := NewRawPack("FILES/x.html", body); ok {
		t.Error("NewRawPack(FILES/x.html) ok=true, want false (not a pack)")
	}
}
