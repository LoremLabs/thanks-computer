package admission

import "testing"

func TestMarkDeniedAndRead(t *testing.T) {
	out := MarkDenied(`{"_txc":{"src":"http"}}`,
		Decision{Admit: false, Status: 402, Reason: "payment_required"}, "acme")
	status, reason, ok := Denied(out)
	if !ok || status != 402 || reason != "payment_required" {
		t.Errorf("Denied = (%d,%q,%v), want (402,payment_required,true)", status, reason, ok)
	}
}

func TestMarkDeniedDefaultStatus(t *testing.T) {
	out := MarkDenied("", Decision{Admit: false}, "")
	if status, _, ok := Denied(out); !ok || status != 403 {
		t.Errorf("default status = %d (ok=%v), want 403", status, ok)
	}
}

func TestDeniedFalseWhenUnmarked(t *testing.T) {
	if _, _, ok := Denied(`{"_txc":{"src":"http"}}`); ok {
		t.Error("Denied must be false for an unmarked envelope")
	}
}
