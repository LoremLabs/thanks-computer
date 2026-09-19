package ui

import (
	"strings"
	"testing"
)

var front = Printer{
	Name:    "front-desk",
	URI:     "ipps://ipp.stacks.example:443/p/core-abc123/front-desk",
	Address: "ipp.stacks.example:443",
	Queue:   "p/core-abc123/front-desk",
}

// Page must always return a usable, on-brand HTML document that names the
// printer — the built bundle with the details injected when present, the
// server-rendered fallback otherwise.
func TestPage(t *testing.T) {
	html, built := Page(front)
	s := string(html)
	if !strings.Contains(strings.ToLower(s), "<!doctype html>") || !strings.Contains(s, "thanks, c") {
		t.Fatalf("not an on-brand HTML page: %.120s", s)
	}
	if built {
		if strings.Contains(s, marker) {
			t.Fatal("built page still carries the injection marker")
		}
		if !strings.Contains(s, `<script id="txco-printer" type="application/json">{"name":"front-desk","uri":"ipps://ipp.stacks.example:443/p/core-abc123/front-desk"`) {
			t.Fatalf("built page lacks the printer's details")
		}
	}
}

// The fallback names the printer and links Add Printer with the ipps:// URI
// itself — html/template would otherwise neuter the unknown scheme.
func TestFallbackPage(t *testing.T) {
	s := string(fallbackPage(front))
	for _, want := range []string{"<title>front-desk · virtual printer</title>", `href="` + front.URI + `"`, front.Address, front.Queue} {
		if !strings.Contains(s, want) {
			t.Errorf("fallback lacks %q", want)
		}
	}
	if strings.Contains(s, "ZgotmplZ") {
		t.Fatal("fallback neutered the ipps:// link")
	}
}

// Whatever a name holds, it cannot break out of the page: escaped in the
// fallback, JSON-escaped inside the injected <script>.
func TestPageEscapes(t *testing.T) {
	evil := front
	evil.Name = `</script><script>alert(1)</script>`
	if s := string(fallbackPage(evil)); strings.Contains(s, "<script>alert") {
		t.Fatal("fallback did not escape the name")
	}
	if html, built := Page(evil); built && strings.Contains(string(html), "</script><script>alert") {
		t.Fatal("injected details closed their <script>")
	}
}
