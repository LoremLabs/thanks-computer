package lmtp

import (
	"testing"

	"github.com/emersion/go-smtp"
	"go.uber.org/zap"
)

// ----- Unit tests for computeVerdicts (pure function, no I/O) -----

// codeFromVerdict picks the code out of a verdict slot. A nil entry
// means "accept" — modeled as 250 so tables read naturally.
func codeFromVerdict(v *smtp.SMTPError) int {
	if v == nil {
		return 250
	}
	return v.Code
}

func TestComputeVerdicts_PerRecipientMix(t *testing.T) {
	rcpts := []string{"a@x", "b@x"}
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.code", 550)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.msg", "no inbox")

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	if got := codeFromVerdict(v[0]); got != 250 {
		t.Errorf("v[0] = %d, want 250", got)
	}
	if got := codeFromVerdict(v[1]); got != 550 {
		t.Errorf("v[1] = %d, want 550", got)
	}
	if v[1] == nil || v[1].Message != "no inbox" {
		t.Errorf("v[1].Message lost: %+v", v[1])
	}
}

func TestComputeVerdicts_AllAcceptArray(t *testing.T) {
	rcpts := []string{"a@x", "b@x", "c@x"}
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.2.code", 250)

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	for i, e := range v {
		if e != nil {
			t.Errorf("v[%d] expected nil (accept), got %+v", i, e)
		}
	}
}

func TestComputeVerdicts_AllRejectArray(t *testing.T) {
	rcpts := []string{"a@x", "b@x"}
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 550)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.code", 550)

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	if codeFromVerdict(v[0]) != 550 || codeFromVerdict(v[1]) != 550 {
		t.Errorf("want both 550, got %d / %d",
			codeFromVerdict(v[0]), codeFromVerdict(v[1]))
	}
}

// TestComputeVerdicts_ShortArray_FillsWith550 — when the rules return
// recipients[] shorter than the actual rcpt list, the missing slots
// MUST default-deny (550) rather than reusing the last entry. This is
// the load-bearing rule of Phase 3: explicit accept is required per
// recipient.
func TestComputeVerdicts_ShortArray_FillsWith550(t *testing.T) {
	rcpts := []string{"a@x", "b@x", "c@x"}
	// Only the first two slots are populated; the third must NOT
	// inherit slot[1]'s 250 — that would let a rule accidentally
	// accept mail for a recipient it never considered.
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.code", 250)

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	if codeFromVerdict(v[0]) != 250 {
		t.Errorf("v[0] = %d, want 250", codeFromVerdict(v[0]))
	}
	if codeFromVerdict(v[1]) != 250 {
		t.Errorf("v[1] = %d, want 250", codeFromVerdict(v[1]))
	}
	if got := codeFromVerdict(v[2]); got != 550 {
		t.Errorf("v[2] = %d, want 550 (default-deny on missing slot, NOT inherited)", got)
	}
}

// TestComputeVerdicts_LongArray_UsesFirstN — when recipients[] has
// more entries than there are rcpts, extras are silently ignored
// (logged but not acted on). Defensive against operator error.
func TestComputeVerdicts_LongArray_UsesFirstN(t *testing.T) {
	rcpts := []string{"a@x"}
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.recipients.1.code", 550) // extra

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	if len(v) != 1 {
		t.Fatalf("verdicts len = %d, want 1", len(v))
	}
	if codeFromVerdict(v[0]) != 250 {
		t.Errorf("v[0] = %d, want 250", codeFromVerdict(v[0]))
	}
}

// TestComputeVerdicts_MissingCode_DefaultDenies — a slot is present
// in the array but has no `code` field. Treated as default-deny
// (NOT as "accept by omission"). Rule authors who write
// `_txc.lmtp.res.recipients.0.msg` without a code get 550.
func TestComputeVerdicts_MissingCode_DefaultDenies(t *testing.T) {
	rcpts := []string{"a@x"}
	raw := mustSet("{}", "_txc.lmtp.res.recipients.0.msg", "oops no code")

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	if got := codeFromVerdict(v[0]); got != 550 {
		t.Errorf("v[0] = %d, want 550 (missing code = default-deny)", got)
	}
}

// TestComputeVerdicts_BroadcastFallback — no recipients[] array but
// the broadcast `_txc.lmtp.res.code` is set: stamp it on every rcpt.
func TestComputeVerdicts_BroadcastFallback(t *testing.T) {
	rcpts := []string{"a@x", "b@x", "c@x"}
	raw := mustSet("{}", "_txc.lmtp.res.code", 250)
	raw = mustSet(raw, "_txc.lmtp.res.msg", "OK")

	v := computeVerdicts(rcpts, raw, zap.NewNop())
	for i, e := range v {
		if e != nil {
			t.Errorf("v[%d] = %+v, want nil (250 broadcast)", i, e)
		}
	}
}

// TestComputeVerdicts_DefaultDenyFallback — neither array nor
// broadcast set: every rcpt defaults to 550. This is the Phase 0
// no-route case, now verified at the unit level too.
func TestComputeVerdicts_DefaultDenyFallback(t *testing.T) {
	rcpts := []string{"a@x", "b@x"}
	v := computeVerdicts(rcpts, "{}", zap.NewNop())
	for i, e := range v {
		if codeFromVerdict(e) != 550 {
			t.Errorf("v[%d] = %d, want 550", i, codeFromVerdict(e))
		}
	}
}

func TestComputeVerdicts_InvalidJSON_DefaultDenies(t *testing.T) {
	rcpts := []string{"a@x"}
	v := computeVerdicts(rcpts, "not json", zap.NewNop())
	if codeFromVerdict(v[0]) != 550 {
		t.Errorf("invalid json should default-deny, got %d", codeFromVerdict(v[0]))
	}
}

// ----- End-to-end tests for LMTPData over the wire -----

// TestLMTPData_PerRecipientMix delivers to two recipients and sets a
// 250/550 mix on the response. The wire-level result must be one
// 250 line for rcpt[0] and one 550 line for rcpt[1], inspected via
// the client's LMTPDataError map.
func TestLMTPData_PerRecipientMix(t *testing.T) {
	resp := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	resp = mustSet(resp, "_txc.lmtp.res.recipients.1.code", 550)
	resp = mustSet(resp, "_txc.lmtp.res.recipients.1.msg", "rejected")

	addr, stop := startTestController(t, fakeResponder{response: resp})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client,
		"alice@example.com",
		[]string{"accept@your.tenant", "reject@your.tenant"},
		"hello")

	if got := codeFor(err, "accept@your.tenant"); got != 250 {
		t.Errorf("rcpt[0] = %d, want 250 (err=%v)", got, err)
	}
	if got := codeFor(err, "reject@your.tenant"); got != 550 {
		t.Errorf("rcpt[1] = %d, want 550 (err=%v)", got, err)
	}
}

// TestLMTPData_ShortArray_OverWire — only the first recipient is
// addressed by the rule; the second must default-deny on the wire.
func TestLMTPData_ShortArray_OverWire(t *testing.T) {
	resp := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	// Deliberately no entry for index 1.

	addr, stop := startTestController(t, fakeResponder{response: resp})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client,
		"alice@example.com",
		[]string{"named@your.tenant", "forgotten@your.tenant"},
		"hi")

	if got := codeFor(err, "named@your.tenant"); got != 250 {
		t.Errorf("rcpt[0] = %d, want 250 (err=%v)", got, err)
	}
	if got := codeFor(err, "forgotten@your.tenant"); got != 550 {
		t.Errorf("rcpt[1] = %d, want 550 default-deny (err=%v)", got, err)
	}
}

// TestLMTPData_BroadcastStillWorks — a rule that uses the plain
// `_txc.lmtp.res.code` (no recipients[] array) still broadcasts to
// every rcpt. Regression test for the Phase 0 contract.
func TestLMTPData_BroadcastStillWorks(t *testing.T) {
	resp := mustSet("{}", "_txc.lmtp.res.code", 451)
	resp = mustSet(resp, "_txc.lmtp.res.msg", "try later")

	addr, stop := startTestController(t, fakeResponder{response: resp})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{"a@your.tenant", "b@your.tenant", "c@your.tenant"}
	err := sendMail(client, "alice@example.com", rcpts, "hi")
	for _, r := range rcpts {
		if got := codeFor(err, r); got != 451 {
			t.Errorf("rcpt %q = %d, want 451 (broadcast)", r, got)
		}
	}
}

// TestLMTPData_AllAcceptArray — rule explicitly accepts each rcpt
// via the recipients[] array. SendMail returns nil (no errors).
func TestLMTPData_AllAcceptArray(t *testing.T) {
	resp := mustSet("{}", "_txc.lmtp.res.recipients.0.code", 250)
	resp = mustSet(resp, "_txc.lmtp.res.recipients.1.code", 250)

	addr, stop := startTestController(t, fakeResponder{response: resp})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client, "alice@example.com",
		[]string{"a@your.tenant", "b@your.tenant"}, "hi")
	if err != nil {
		t.Errorf("expected accept-all to succeed, got %v", err)
	}
}

// TestLMTPData_DefaultDenyOverWire — no rule sets any verdict.
// Every rcpt gets 550 on the wire (Phase 0 default-deny, now via
// LMTPData rather than Data; same outcome).
func TestLMTPData_DefaultDenyOverWire(t *testing.T) {
	addr, stop := startTestController(t, fakeResponder{response: "{}"})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{"x@your.tenant", "y@your.tenant"}
	err := sendMail(client, "alice@example.com", rcpts, "hi")
	for _, r := range rcpts {
		if got := codeFor(err, r); got != 550 {
			t.Errorf("rcpt %q = %d, want 550 (default-deny)", r, got)
		}
	}
}

// TestLMTPData_TimeoutBroadcastsAcrossRecipients — pipeline never
// responds; transport-level 451 broadcasts to every rcpt (we can't
// know per-recipient verdicts when the pipeline never delivered one).
//
// Inlined setup (vs. startTestController) so the bus reader can
// deliberately swallow without responding — the standard helper's
// fakeResponder always writes back.
func TestLMTPData_TimeoutBroadcastsAcrossRecipients(t *testing.T) {
	addr, stop := startSilentController(t, "100ms")
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{"a@x", "b@x", "c@x"}
	err := sendMail(client, "alice@example.com", rcpts, "hi")
	for _, r := range rcpts {
		if got := codeFor(err, r); got != 451 {
			t.Errorf("rcpt %q = %d, want 451 timeout (err=%v)", r, got, err)
		}
	}
}
