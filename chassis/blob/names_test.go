package blob

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	ok := []string{
		"faqs/house-01.doc",
		"bookings/2026.csv",
		"a",
		"docs/" + strings.Repeat("a", 128),
		"paris/docs/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x.y.z",
	}
	for _, n := range ok {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", n, err)
		}
	}
	bad := map[string]string{
		"":                   "empty",
		"/faqs/x":            "leading slash",
		"faqs/x/":            "trailing slash",
		"faqs//x":            "doubled slash",
		"../x":               "dotdot",
		"faqs/./x":           "dot segment",
		"_meta/x":            "reserved leading underscore",
		"faqs/_x":            "reserved leading underscore (nested)",
		"faqs/ho use.doc":    "space",
		"faqs/x:y":           "colon",
		"faqs/é.doc":         "non-ascii",
		"x/" + strings.Repeat("a", 129): "segment too long",
		strings.Repeat("a", 120) + "/" + strings.Repeat("b", 120) + "/" + strings.Repeat("c", 12): "251 bytes total",
	}
	for n, why := range bad {
		if err := ValidName(n); err == nil {
			t.Errorf("ValidName(%q) = nil, want error (%s)", n, why)
		}
	}
	// 250 bytes exactly is fine.
	n250 := strings.Repeat("a", 120) + "/" + strings.Repeat("b", 120) + "/" + strings.Repeat("c", 8)
	if len(n250) != 250 {
		t.Fatalf("fixture is %d bytes", len(n250))
	}
	if err := ValidName(n250); err != nil {
		t.Errorf("250-byte name rejected: %v", err)
	}
}

func TestValidFilename(t *testing.T) {
	for _, f := range []string{"Menu (v2).pdf", "café.md", "🎉.txt", strings.Repeat("x", 255)} {
		if err := ValidFilename(f); err != nil {
			t.Errorf("ValidFilename(%q) = %v", f, err)
		}
	}
	for _, f := range []string{"", "a\x00b", "tab\there", strings.Repeat("x", 256), "bad\xffutf8"} {
		if err := ValidFilename(f); err == nil {
			t.Errorf("ValidFilename(%q) = nil, want error", f)
		}
	}
}

func TestValidSha256(t *testing.T) {
	good := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if !ValidSha256(good) {
		t.Error("empty-content sha rejected")
	}
	for _, s := range []string{"", good[:63], strings.ToUpper(good), good[:63] + "g"} {
		if ValidSha256(s) {
			t.Errorf("ValidSha256(%q) = true", s)
		}
	}
}

func TestDefaultContentType(t *testing.T) {
	if ct := DefaultContentType("a/b/menu.pdf"); ct != "application/pdf" {
		t.Errorf("pdf → %q", ct)
	}
	if ct := DefaultContentType("noext"); ct != "application/octet-stream" {
		t.Errorf("noext → %q", ct)
	}
}
