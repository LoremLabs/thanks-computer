package processor

import "testing"

// TestTenantFromEnvelope: reads the resolved tenant slug off `_txc.tenant`
// (what trace-usage emission attributes a trace to). Mirrors
// FuelUsedFromEnvelope.
func TestTenantFromEnvelope(t *testing.T) {
	cases := map[string]string{
		`{"_txc":{"tenant":"prod-mankins"}}`: "prod-mankins",
		`{"_txc":{"tenant":"_sys"}}`:         "_sys",
		`{"_txc":{}}`:                        "",
		`{}`:                                 "",
		``:                                   "",
	}
	for raw, want := range cases {
		if got := TenantFromEnvelope(raw); got != want {
			t.Errorf("TenantFromEnvelope(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestWebHostFromEnvelope: reads the client's Host header off
// `_txc.web.req.host` (what the usage line reports as web.host). Every
// non-HTTP source has no such path and must come back empty, not blank-ish.
func TestWebHostFromEnvelope(t *testing.T) {
	cases := map[string]string{
		`{"_txc":{"web":{"req":{"host":"www.dripl.it"}}}}`:          "www.dripl.it",
		`{"_txc":{"web":{"req":{"host":"a.stacks.example:8443"}}}}`: "a.stacks.example:8443",
		`{"_txc":{"web":{"req":{}}}}`:                               "",
		`{"_txc":{"src":"mail"}}`:                                   "",
		`{}`:                                                        "",
		``:                                                          "",
	}
	for raw, want := range cases {
		if got := WebHostFromEnvelope(raw); got != want {
			t.Errorf("WebHostFromEnvelope(%q) = %q, want %q", raw, got, want)
		}
	}
}
