package blob

import (
	"strings"
	"testing"
)

func TestDerivedNameTotalAndNFC(t *testing.T) {
	nfc := "café menu (v2).pdf"       // é precomposed
	nfd := "café menu (v2).pdf"      // e + combining acute (macOS Finder)
	a, err := DerivedName("paris/docs", nfc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DerivedName("paris/docs", nfd)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("NFC/NFD filenames derived different names:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(a, "paris/docs/") || len(a) != len("paris/docs/")+64 {
		t.Fatalf("derived shape: %q", a)
	}
	if err := ValidName(a); err != nil {
		t.Fatalf("derived name invalid: %v", err)
	}
	// Any real filename works; different filenames differ.
	c, err := DerivedName("paris/docs", "🎉 totally different.pdf")
	if err != nil || c == a {
		t.Fatalf("emoji filename: %q err=%v", c, err)
	}
	// Bad inputs.
	if _, err := DerivedName("_x", "a.pdf"); err == nil {
		t.Error("reserved under accepted")
	}
	if _, err := DerivedName("paris/docs", ""); err == nil {
		t.Error("empty filename accepted")
	}
}
