package ipp

import (
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

var fixedNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func attrByName(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}

// TestPrinterIsLeastFictional pins the rule: every physical capability is
// advertised as exactly ONE value. A second value is a choice, and a choice
// TxCo ignores is a lie told to the user's print dialog.
func TestPrinterIsLeastFictional(t *testing.T) {
	attrs := PrinterAttributes(PrinterInfo{
		Tenant: "acme", Name: "research", URI: "ipps://ipp.example.com/p/research",
		Secure: true, UpSince: fixedNow.Add(-time.Hour),
	}, fixedNow)

	for _, name := range []string{
		"media-supported", "media-default", "sides-supported", "print-color-mode-supported",
		"print-quality-supported", "printer-resolution-supported", "orientation-requested-supported",
		"copies-supported", "copies-default", "media-col-default",
	} {
		a, ok := attrByName(attrs, name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if len(a.Values) != 1 {
			t.Errorf("%s offers %d values; a fictional capability must offer exactly one", name, len(a.Values))
		}
	}
	// Exactly one copy: a range of 1-1, not 1-99.
	c, _ := attrByName(attrs, "copies-supported")
	if r, ok := c.Values[0].V.(goipp.Range); !ok || r.Lower != 1 || r.Upper != 1 {
		t.Fatalf("copies-supported = %v, want 1-1", c.Values[0].V)
	}
	// No job-template attribute is claimed as settable.
	jc, _ := attrByName(attrs, "job-creation-attributes-supported")
	for _, v := range jc.Values {
		switch v.V.String() {
		case "job-name", "document-name", "document-format":
		default:
			t.Errorf("job-creation-attributes-supported claims %q", v.V)
		}
	}

	// The real things are real.
	if a, _ := attrByName(attrs, "document-format-supported"); len(a.Values) != 1 || a.Values[0].V.String() != FormatPDF {
		t.Fatalf("default formats: %v", a.Values)
	}
	if a, _ := attrByName(attrs, "uri-security-supported"); a.Values[0].V.String() != "tls" {
		t.Fatalf("uri-security-supported: %v", a.Values)
	}
	if a, _ := attrByName(attrs, "printer-up-time"); a.Values[0].V.(goipp.Integer) != 3600 {
		t.Fatalf("printer-up-time: %v", a.Values)
	}
	if a, _ := attrByName(attrs, "operations-supported"); len(a.Values) != len(SupportedOperations) {
		t.Fatalf("operations-supported: %v", a.Values)
	}

	// The whole set must survive the wire.
	resp := Response(nil, goipp.StatusOk)
	resp.Printer = attrs
	if _, err := resp.EncodeBytes(); err != nil {
		t.Fatalf("printer attributes do not encode: %v", err)
	}
}

// A registered printer's display name is what a client names the queue. With
// none, printer-info keeps its label form and printer-dns-sd-name is absent
// (an empty name would be a worse answer than none).
func TestPrinterDisplayName(t *testing.T) {
	base := PrinterInfo{Tenant: "acme", Name: "paris", URI: "ipps://ipp.example.com/p/paris", UpSince: fixedNow}

	attrs := PrinterAttributes(base, fixedNow)
	if a, _ := attrByName(attrs, "printer-info"); a.Values[0].V.String() != "Print to paris" {
		t.Fatalf("printer-info without a display name: %v", a.Values)
	}
	if _, ok := attrByName(attrs, "printer-dns-sd-name"); ok {
		t.Fatal("printer-dns-sd-name sent for a printer with no display name")
	}

	base.DisplayName = "Paris"
	attrs = PrinterAttributes(base, fixedNow)
	if a, _ := attrByName(attrs, "printer-info"); a.Values[0].V.String() != "Paris" {
		t.Fatalf("printer-info: %v", a.Values)
	}
	if a, ok := attrByName(attrs, "printer-dns-sd-name"); !ok || a.Values[0].V.String() != "Paris" {
		t.Fatalf("printer-dns-sd-name: %v %v", ok, a.Values)
	}
	// printer-name stays the label: it is part of the printer's URI identity.
	if a, _ := attrByName(attrs, "printer-name"); a.Values[0].V.String() != "paris" {
		t.Fatalf("printer-name: %v", a.Values)
	}
}

func TestPrinterUUIDStable(t *testing.T) {
	a, b := PrinterUUID("acme", "research"), PrinterUUID("acme", "research")
	if a != b || len(a) != len("urn:uuid:00000000-0000-0000-0000-000000000000") {
		t.Fatalf("uuid not stable/well-formed: %q %q", a, b)
	}
	if PrinterUUID("acme", "other") == a || PrinterUUID("beta", "research") == a {
		t.Fatal("uuid must differ per tenant and per printer")
	}
}

// IPP `completed` is `delivered`: the print job ends when TxCo accepts the
// document, whatever the stack does with it afterwards.
func TestJobStateMapping(t *testing.T) {
	cases := map[string]int{
		StateReceiving: 3, StateCommitted: 5, StateDelivered: 9, StateCanceled: 7, StateFailed: 8,
	}
	for state, want := range cases {
		if got, reason := JobState(state); got != want || reason == "" {
			t.Errorf("JobState(%s) = %d %q, want %d", state, got, reason, want)
		}
	}
	j := Job{Number: 7, State: StateDelivered, JobName: "x", Size: 2049, CreatedAt: fixedNow, DeliveredAt: fixedNow.Add(time.Second)}
	attrs := JobAttributes(j, "ipps://ipp.example.com/p/research", fixedNow.Add(-time.Minute))
	if a, _ := attrByName(attrs, "job-uri"); a.Values[0].V.String() != "ipps://ipp.example.com/p/research/jobs/7" {
		t.Fatalf("job-uri: %v", a.Values)
	}
	if a, _ := attrByName(attrs, "job-k-octets"); a.Values[0].V.(goipp.Integer) != 3 {
		t.Fatalf("job-k-octets: %v", a.Values)
	}
	if _, ok := attrByName(attrs, "time-at-completed"); !ok {
		t.Fatal("terminal job lacks time-at-completed")
	}
}
