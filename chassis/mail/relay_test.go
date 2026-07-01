package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// A representative inbound RFC 5322 message — the bytes a real `.forward` must
// relay untouched (note the original From + a DKIM-Signature header we must not
// disturb).
var rawInbound = []byte("From: Alice <alice@example.com>\r\n" +
	"To: matt@dripl.it\r\n" +
	"Subject: hello there\r\n" +
	"DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=sel; b=abc\r\n" +
	"\r\n" +
	"Body of the original message.\r\n")

// relayEnv builds an `in` envelope with a `_relay` block from key→value.
func relayEnv(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	in := "{}"
	for k, v := range fields {
		var err error
		in, err = sjson.Set(in, "_relay."+k, v)
		if err != nil {
			t.Fatalf("sjson set %s: %v", k, err)
		}
	}
	return []byte(in)
}

func goodRelayFields() map[string]any {
	return map[string]any{
		"raw":           base64.StdEncoding.EncodeToString(rawInbound),
		"to":            "mankins@gmail.com",
		"envelope_from": "noreply@acme.com", // acme.com is a verified domain in the test DB
		"campaign":      "fwd:<msg-1@example.com>",
	}
}

func TestRelayVerbatim(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	u := &fakeUsage{}
	m := newTestMailer(t, db, sub, u)

	p, err := m.Relay(context.Background(), "acme", "lmtp", relayEnv(t, goodRelayFields()))
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if got := gjson.Get(p.Raw, "_relay.result.status").String(); got != "sent" {
		t.Fatalf("status=%q want sent; raw=%s", got, p.Raw)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("submit calls=%d want 1", len(sub.calls))
	}
	c := sub.calls[0]
	// Envelope rewritten, recipient is the forward target.
	if c.from != "noreply@acme.com" || len(c.rcpts) != 1 || c.rcpts[0] != "mankins@gmail.com" {
		t.Fatalf("submit from/rcpts = %q/%v", c.from, c.rcpts)
	}
	// The bytes must be the ORIGINAL message, unchanged (no re-compose, no re-sign).
	if !bytes.Equal(c.msg, rawInbound) {
		t.Fatalf("relayed bytes were modified:\ngot:  %q\nwant: %q", c.msg, rawInbound)
	}
	if len(u.events) != 1 || u.events[0].Src != "relay" || !u.events[0].Billable {
		t.Fatalf("usage events = %+v", u.events)
	}
}

func TestRelayForbiddenSource(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	// The security boundary: anything other than the inbound-mail path is refused
	// BEFORE any submit — a coerced web pipeline can't use relay as a spam cannon.
	for _, src := range []string{"http", "cron", "", "LMTP"} {
		p, err := m.Relay(context.Background(), "acme", src, relayEnv(t, goodRelayFields()))
		if err == nil {
			t.Fatalf("source=%q: expected error", src)
		}
		if got := gjson.Get(p.Raw, "_relay.result.reason").String(); got != "forbidden_source" {
			t.Fatalf("source=%q: reason=%q want forbidden_source", src, got)
		}
	}
	if len(sub.calls) != 0 {
		t.Fatalf("submit called %d times; must be 0 when source is forbidden", len(sub.calls))
	}
}

func TestRelayEnvelopeFromNotVerified(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	f := goodRelayFields()
	f["envelope_from"] = "noreply@evil.example" // not a verified domain for acme
	p, err := m.Relay(context.Background(), "acme", "lmtp", relayEnv(t, f))
	if err == nil {
		t.Fatal("expected error for unverified envelope_from")
	}
	if got := gjson.Get(p.Raw, "_relay.result.reason").String(); got != "envelope_from_not_verified" {
		t.Fatalf("reason=%q want envelope_from_not_verified", got)
	}
	if len(sub.calls) != 0 {
		t.Fatalf("submit called %d times; must be 0 when envelope_from is unverified", len(sub.calls))
	}
}

func TestRelayMissingFields(t *testing.T) {
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})

	for name, drop := range map[string]string{"raw": "raw", "to": "to", "envelope_from": "envelope_from"} {
		f := goodRelayFields()
		delete(f, drop)
		if _, err := m.Relay(context.Background(), "acme", "lmtp", relayEnv(t, f)); err == nil {
			t.Fatalf("missing %s: expected error", name)
		}
	}
}

func TestRelayInvalidBase64(t *testing.T) {
	db := newTestDB(t)
	m := newTestMailer(t, db, &fakeSubmit{}, &fakeUsage{})
	f := goodRelayFields()
	f["raw"] = "!!! not base64 !!!"
	p, err := m.Relay(context.Background(), "acme", "lmtp", relayEnv(t, f))
	if err == nil {
		t.Fatal("expected error for invalid base64 raw")
	}
	if got := gjson.Get(p.Raw, "_relay.result.reason").String(); got != "invalid_raw" {
		t.Fatalf("reason=%q want invalid_raw", got)
	}
}

func TestRelayCampaignDedup(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{}
	m := newTestMailer(t, db, sub, &fakeUsage{})

	in := relayEnv(t, goodRelayFields())
	// First forward sends.
	if _, err := m.Relay(context.Background(), "acme", "lmtp", in); err != nil {
		t.Fatalf("first relay: %v", err)
	}
	// A redelivery of the SAME message (same campaign key) is a no-op — the
	// reader isn't double-forwarded.
	p, err := m.Relay(context.Background(), "acme", "lmtp", in)
	if err != nil {
		t.Fatalf("second relay: %v", err)
	}
	if got := gjson.Get(p.Raw, "_relay.result.status").String(); got != "skipped" {
		t.Fatalf("status=%q want skipped", got)
	}
	if len(sub.calls) != 1 {
		t.Fatalf("submit calls=%d want 1 (dedup)", len(sub.calls))
	}
}

func TestRelayReleasesClaimOnSubmitFailure(t *testing.T) {
	db := newTestDB(t)
	sub := &fakeSubmit{err: context.DeadlineExceeded} // submit always fails
	m := newTestMailer(t, db, sub, &fakeUsage{})

	in := relayEnv(t, goodRelayFields())
	if _, err := m.Relay(context.Background(), "acme", "lmtp", in); err == nil {
		t.Fatal("expected submit error")
	}
	// The claim was released, so a retry (now with a working relay) re-forwards.
	sub.err = nil
	p, err := m.Relay(context.Background(), "acme", "lmtp", in)
	if err != nil {
		t.Fatalf("retry relay: %v", err)
	}
	if got := gjson.Get(p.Raw, "_relay.result.status").String(); got != "sent" {
		t.Fatalf("retry status=%q want sent (claim should have been released)", got)
	}
}
