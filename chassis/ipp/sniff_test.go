package ipp

import (
	"bytes"
	"testing"
)

func TestSniff(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"pdf", []byte("%PDF-1.7\n%âãÏÓ\n"), FormatPDF},
		{"pdf after junk", append(bytes.Repeat([]byte{0}, 200), []byte("%PDF-1.4")...), FormatPDF},
		{"pdf marker past the window", append(bytes.Repeat([]byte("x"), SniffBytes), []byte("%PDF-1.4")...), FormatText},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10}, FormatJPEG},
		{"pwg", []byte("RaS2PwgRaster\x00"), FormatPWGRaster},
		{"urf", []byte("UNIRAST\x00\x00\x00\x00\x01"), FormatURF},
		{"postscript", []byte("%!PS-Adobe-3.0\n"), "application/postscript"},
		{"text", []byte("hello, pony\n"), FormatText},
		{"binary junk", []byte{0x00, 0x01, 0x02, 0xFE}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := Sniff(c.head); got != c.want {
			t.Errorf("%s: Sniff = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResolve(t *testing.T) {
	pdf := []byte("%PDF-1.7\n")
	ps := []byte("%!PS-Adobe-3.0\n")
	pdfOnly := []string{FormatPDF}
	withOctet := []string{FormatPDF, FormatOctet}

	cases := []struct {
		name     string
		declared string
		head     []byte
		allowed  []string
		want     string
		ok       bool
	}{
		{"declared pdf, is pdf", FormatPDF, pdf, pdfOnly, FormatPDF, true},
		{"declared pdf, is postscript", FormatPDF, ps, pdfOnly, FormatPDF, false},
		{"nothing declared → the default", "", pdf, pdfOnly, FormatPDF, true},
		{"nothing declared, not the default", "", ps, pdfOnly, FormatPDF, false},
		{"postscript is never allowed", "application/postscript", ps, pdfOnly, "", false},
		{"octet-stream not listed", FormatOctet, pdf, pdfOnly, "", false},
		{"octet-stream auto-senses pdf", FormatOctet, pdf, withOctet, FormatPDF, true},
		{"octet-stream cannot smuggle postscript", FormatOctet, ps, withOctet, "", false},
		{"octet-stream of junk", FormatOctet, []byte{0, 1, 2}, withOctet, "", false},
	}
	for _, c := range cases {
		got, ok := Resolve(c.declared, c.head, c.allowed)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: Resolve = %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
	if FormatAllowed("application/postscript", withOctet) {
		t.Fatal("postscript passed the pre-read gate")
	}
	if !FormatAllowed("", pdfOnly) {
		t.Fatal("an absent document-format must pass the pre-read gate")
	}
}
