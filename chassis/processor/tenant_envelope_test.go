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
