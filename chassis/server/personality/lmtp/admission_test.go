package lmtp

import "testing"

// A suspended / over-limit tenant (402) and a draining node (503) map to a
// TEMPORARY 451 so the sender's MTA queues and retries instead of bouncing.
func TestComputeVerdictsAdmission451Default(t *testing.T) {
	raw := `{"_txc":{"admission":{"denied":true,"status":402,"reason":"payment_required"}}}`
	v := computeVerdicts([]string{"a@x.com", "b@x.com"}, raw, nil)
	if len(v) != 2 {
		t.Fatalf("want 2 verdicts, got %d", len(v))
	}
	for i, se := range v {
		if se == nil || se.Code != 451 {
			t.Errorf("rcpt %d: code = %v, want 451", i, se)
		}
	}
}

// A hard-disabled / forbidden tenant (403) maps to a PERMANENT 550.
func TestComputeVerdictsAdmission550WhenForbidden(t *testing.T) {
	raw := `{"_txc":{"admission":{"denied":true,"status":403,"reason":"suspended"}}}`
	v := computeVerdicts([]string{"a@x.com"}, raw, nil)
	if v[0] == nil || v[0].Code != 550 {
		t.Errorf("code = %v, want 550", v[0])
	}
}

// Without an admission marker, the normal rule-verdict path still applies.
func TestComputeVerdictsNoAdmissionUsesRuleVerdict(t *testing.T) {
	raw := `{"_txc":{"lmtp":{"res":{"code":250}}}}`
	if v := computeVerdicts([]string{"a@x.com"}, raw, nil); v[0] != nil {
		t.Errorf("250 should accept (nil verdict), got %v", v[0])
	}
}
