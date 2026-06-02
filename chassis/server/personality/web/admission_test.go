package web

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyAdmissionDeny(t *testing.T) {
	in := `{"_txc":{"admission":{"denied":true,"status":402,"reason":"payment_required"}}}`
	out := applyAdmission(in)
	if got := gjson.Get(out, "_txc.web.res.status").Int(); got != 402 {
		t.Errorf("status = %d, want 402", got)
	}
	if got := gjson.Get(out, "_txc.web.res.headers.x-txc-deny-reason.0").String(); got != "payment_required" {
		t.Errorf("deny-reason = %q, want payment_required", got)
	}
	b, _ := base64.StdEncoding.DecodeString(gjson.Get(out, "_txc.web.res.body").String())
	if string(b) != "402 Payment Required\n" {
		t.Errorf("body = %q, want %q", string(b), "402 Payment Required\n")
	}
}

func TestApplyAdmissionDrainAddsRetryAfter(t *testing.T) {
	in := `{"_txc":{"admission":{"denied":true,"status":503,"reason":"draining"}}}`
	out := applyAdmission(in)
	if got := gjson.Get(out, "_txc.web.res.status").Int(); got != 503 {
		t.Errorf("status = %d, want 503", got)
	}
	if got := gjson.Get(out, "_txc.web.res.headers.retry-after.0").String(); got != "0" {
		t.Errorf("retry-after = %q, want 0", got)
	}
	if got := gjson.Get(out, "_txc.web.res.headers.connection.0").String(); got != "close" {
		t.Errorf("connection = %q, want close", got)
	}
}

func TestApplyAdmissionNoMarkerNoop(t *testing.T) {
	in := `{"_txc":{"web":{"res":{"status":200}}}}`
	if out := applyAdmission(in); out != in {
		t.Errorf("no marker should be a no-op; got %s", out)
	}
}

func TestApplyAdmissionRespectsExplicitStatus(t *testing.T) {
	// A stack that already shaped its own status wins over the marker.
	in := `{"_txc":{"admission":{"denied":true,"status":402},"web":{"res":{"status":418}}}}`
	if got := gjson.Get(applyAdmission(in), "_txc.web.res.status").Int(); got != 418 {
		t.Errorf("status = %d, want 418 (explicit response wins)", got)
	}
}
