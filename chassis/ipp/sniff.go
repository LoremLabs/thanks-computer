package ipp

import (
	"bytes"
	"unicode/utf8"
)

// SniffBytes is how much of a document's head the inlet looks at.
const SniffBytes = 1024

// Document formats this package can recognise. application/postscript is
// deliberately absent: PostScript is a programming language, not a passive
// page format, and the inlet never accepts it.
const (
	FormatPDF       = "application/pdf"
	FormatJPEG      = "image/jpeg"
	FormatPWGRaster = "image/pwg-raster"
	FormatURF       = "image/urf"
	FormatText      = "text/plain"
	FormatOctet     = "application/octet-stream" // "the printer should auto-sense"
)

// Sniff names the format a document head plausibly is, or "" when it is
// none this package knows. Shallow on purpose: it answers "does the payload
// resemble what the client claimed", not "is this a safe PDF" — deep parsing
// belongs downstream, and the stack must still treat the bytes as hostile.
func Sniff(head []byte) string {
	if len(head) > SniffBytes {
		head = head[:SniffBytes]
	}
	switch {
	// The PDF spec tolerates leading junk: the marker must appear within the
	// first 1024 bytes, not necessarily at offset 0.
	case bytes.Contains(head, []byte("%PDF-")):
		return FormatPDF
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG
	case bytes.HasPrefix(head, []byte("RaS2")):
		return FormatPWGRaster
	case bytes.HasPrefix(head, []byte("UNIRAST\x00")):
		return FormatURF
	case bytes.HasPrefix(head, []byte("%!")):
		return "application/postscript" // recognised only so it can be refused
	case len(head) > 0 && utf8.Valid(trimPartialRune(head)) && !bytes.ContainsRune(head, 0):
		return FormatText
	}
	return ""
}

// trimPartialRune drops a multi-byte rune cut off by the sniff window so a
// valid UTF-8 document is not judged invalid at the boundary.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// FormatAllowed is the pre-read gate: may a job declaring this
// document-format be accepted at all? An absent declaration means "the
// printer's default" (allowed[0]) and is always acceptable.
func FormatAllowed(declared string, allowed []string) bool {
	if declared == "" {
		return len(allowed) > 0
	}
	for _, a := range allowed {
		if a == declared {
			return true
		}
	}
	return false
}

// Resolve decides the format a job is recorded under once the document's
// head has been read. ok=false means the payload obviously is not what was
// declared (IPP client-error-document-format-error).
//
//	declared ""              → the printer default (allowed[0]); head must agree
//	declared octet-stream    → auto-sense: whatever the head is, if THAT is allowed
//	declared anything else   → head must agree
//
// Callers gate on FormatAllowed first; Resolve re-checks so it is safe alone.
func Resolve(declared string, head []byte, allowed []string) (format string, ok bool) {
	if !FormatAllowed(declared, allowed) {
		return "", false
	}
	sniffed := Sniff(head)
	switch declared {
	case "":
		declared = allowed[0]
	case FormatOctet:
		if sniffed == "" || sniffed == FormatOctet || !FormatAllowed(sniffed, allowed) {
			return "", false
		}
		return sniffed, true
	}
	return declared, sniffed == declared
}
