package ipp

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/OpenPrinting/goipp"
)

// printJob builds a Print-Job request the way a client does: the IPP
// attribute block, then the raw document straight after it.
func printJob(t *testing.T, doc []byte, jobAttrs ...goipp.Attribute) []byte {
	t.Helper()
	req := goipp.NewRequest(goipp.MakeVersion(2, 0), goipp.OpPrintJob, 42)
	req.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	req.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	req.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipps://ipp.example.com/p/research")))
	req.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("matt")))
	req.Operation.Add(goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("Quarterly report")))
	req.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")))
	for _, a := range jobAttrs {
		req.Job.Add(a)
	}
	head, err := req.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	return append(head, doc...)
}

// TestDecodeLeavesDocumentUnread is the assumption the whole streaming path
// rests on: decoding the IPP header consumes EXACTLY the header, so the
// document behind it can be streamed into the CAS without being buffered.
// Checked against a reader that returns one byte per Read too — the
// worst case for a decoder that over-reads.
func TestDecodeLeavesDocumentUnread(t *testing.T) {
	doc := bytes.Repeat([]byte("%PDF-1.7 not really a pdf\n"), 4096) // ~100 KiB, larger than any internal buffer
	wire := printJob(t, doc)

	for name, r := range map[string]io.Reader{
		"plain":    bytes.NewReader(wire),
		"one-byte": iotest.OneByteReader(bytes.NewReader(wire)),
	} {
		t.Run(name, func(t *testing.T) {
			msg, err := DecodeRequest(r, 0)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if goipp.Op(msg.Code) != goipp.OpPrintJob || msg.RequestID != 42 {
				t.Fatalf("decoded op=%v id=%d", goipp.Op(msg.Code), msg.RequestID)
			}
			rest, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rest, doc) {
				t.Fatalf("document after the header: got %d bytes, want %d (decoder over- or under-read)", len(rest), len(doc))
			}
		})
	}
}

func TestDecodeRequestBoundsAndVersion(t *testing.T) {
	// A header longer than the cap is refused rather than built.
	if _, err := DecodeRequest(bytes.NewReader(printJob(t, nil)), 16); err == nil {
		t.Fatal("header over the cap decoded")
	}
	// IPP 3.0 does not exist; refuse a major we do not speak.
	req := goipp.NewRequest(goipp.MakeVersion(3, 0), goipp.OpGetPrinterAttributes, 1)
	b, _ := req.EncodeBytes()
	if _, err := DecodeRequest(bytes.NewReader(b), 0); !errors.Is(err, ErrVersion) {
		t.Fatalf("version 3.0: err=%v, want ErrVersion", err)
	}
	// 1.1 (CUPS's default) is fine.
	req = goipp.NewRequest(goipp.MakeVersion(1, 1), goipp.OpGetPrinterAttributes, 1)
	b, _ = req.EncodeBytes()
	if _, err := DecodeRequest(bytes.NewReader(b), 0); err != nil {
		t.Fatalf("version 1.1: %v", err)
	}
}

func TestRequestAccessorsAndResponse(t *testing.T) {
	msg, err := DecodeRequest(bytes.NewReader(printJob(t, nil,
		goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(17)),
		goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String("iso_a4_210x297mm")))), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !CharsetFirst(msg) {
		t.Fatal("CharsetFirst false for a well-formed request")
	}
	if v, ok := OpString(msg, "job-name"); !ok || v != "Quarterly report" {
		t.Fatalf("job-name: %q %v", v, ok)
	}
	if _, ok := OpString(msg, "absent"); ok {
		t.Fatal("absent attribute reported present")
	}

	// 17 copies on A4 means nothing here: both come back as ignored.
	ign := Ignored(msg)
	if len(ign) != 2 || ign[0].Name != "copies" || ign[0].Values[0].T != goipp.TagUnsupportedValue {
		t.Fatalf("Ignored: %v", ign)
	}
	// Asking for exactly the one value the printer has is not asking for
	// anything: the default job ticket (1 copy, letter, one-sided…) is plain OK.
	plain, err := DecodeRequest(bytes.NewReader(printJob(t, nil,
		goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(1)),
		goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String("na_letter_8.5x11in")),
		goipp.MakeAttribute("sides", goipp.TagKeyword, goipp.String("one-sided")),
		goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))), 0)
	if err != nil {
		t.Fatal(err)
	}
	if ign := Ignored(plain); len(ign) != 0 {
		t.Fatalf("the default ticket was reported as ignored: %v", ign)
	}
	// An attribute the printer has never heard of is always ignored.
	odd, _ := DecodeRequest(bytes.NewReader(printJob(t, nil,
		goipp.MakeAttribute("finishings", goipp.TagEnum, goipp.Integer(4)))), 0)
	if ign := Ignored(odd); len(ign) != 1 || ign[0].Name != "finishings" {
		t.Fatalf("unknown attribute: %v", ign)
	}

	resp := Response(msg, goipp.StatusOk)
	if resp.RequestID != 42 || resp.Version != goipp.MakeVersion(2, 0) {
		t.Fatalf("response id/version: %d %v", resp.RequestID, resp.Version)
	}
	if len(resp.Operation) < 2 || resp.Operation[0].Name != "attributes-charset" || resp.Operation[1].Name != "attributes-natural-language" {
		t.Fatalf("response must open with charset + language: %v", resp.Operation)
	}
	if _, err := resp.EncodeBytes(); err != nil {
		t.Fatalf("response does not encode: %v", err)
	}
}

func TestFilterRequested(t *testing.T) {
	attrs := PrinterAttributes(PrinterInfo{Tenant: "acme", Name: "research", URI: "ipps://ipp.example.com/p/research", Secure: true}, fixedNow)
	if got := FilterRequested(attrs, nil); len(got) != len(attrs) {
		t.Fatal("no requested-attributes must return everything")
	}
	if got := FilterRequested(attrs, []string{"all"}); len(got) != len(attrs) {
		t.Fatal("'all' must return everything")
	}
	if got := FilterRequested(attrs, []string{"printer-description"}); len(got) != len(attrs) {
		t.Fatal("a group keyword must return everything")
	}
	got := FilterRequested(attrs, []string{"printer-name", "copies-supported", "no-such-attribute"})
	if len(got) != 2 {
		t.Fatalf("named attributes: %v", got)
	}
}

// RFC 8011 §4.1.3 group order — operation, unsupported, then the object —
// which goipp's named fields do not produce by themselves and CUPS enforces.
func TestOrderedPutsUnsupportedBeforeJob(t *testing.T) {
	resp := Response(nil, goipp.StatusOkIgnoredOrSubstituted)
	resp.Job = goipp.Attributes{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(1))}
	resp.Unsupported = goipp.Attributes{goipp.MakeAttribute("copies", goipp.TagUnsupportedValue, goipp.Void{})}
	wire, err := Ordered(resp).EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	var back goipp.Message
	if err := back.DecodeBytes(wire); err != nil {
		t.Fatal(err)
	}
	var tags []goipp.Tag
	for _, g := range back.Groups {
		tags = append(tags, g.Tag)
	}
	want := []goipp.Tag{goipp.TagOperationGroup, goipp.TagUnsupportedGroup, goipp.TagJobGroup}
	if len(tags) != len(want) {
		t.Fatalf("groups = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("groups = %v, want %v", tags, want)
		}
	}
	// Explicit groups (Get-Jobs) are not rearranged.
	explicit := Response(nil, goipp.StatusOk)
	explicit.Groups = goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: explicit.Operation}, {Tag: goipp.TagJobGroup}, {Tag: goipp.TagJobGroup}}
	if got := Ordered(explicit); len(got.Groups) != 3 {
		t.Fatalf("explicit groups were rebuilt: %v", got.Groups)
	}
}
