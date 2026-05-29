package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
)

func TestBasicAuthEncodeRFC7617(t *testing.T) {
	// RFC 7617 example: Aladdin:open sesame → QWxhZGRpbjpvcGVuIHNlc2FtZQ==
	meta := `{"secrets":{"password":{"secret":"P"}},"user":"Aladdin"}`
	ctx := withBagAndMeta(t, "P", []byte("open sesame"), meta)

	out, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := gjson.Get(out.Raw, "_txc.computed.basic_auth").String()
	want := "QWxhZGRpbjpvcGVuIHNlc2FtZQ=="
	if got != want {
		t.Errorf("got %q, want %q (RFC 7617 example)", got, want)
	}
}

func TestBasicAuthEncodeCustomOutputPath(t *testing.T) {
	meta := `{"secrets":{"password":{"secret":"P"}},"user":"AC123","output_path":"_txc.computed.twilio_auth"}`
	ctx := withBagAndMeta(t, "P", []byte("token"), meta)

	out, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !gjson.Get(out.Raw, "_txc.computed.twilio_auth").Exists() {
		t.Errorf("custom output_path not honored: %s", out.Raw)
	}
	if gjson.Get(out.Raw, "_txc.computed.basic_auth").Exists() {
		t.Errorf("default path should NOT be populated when custom set: %s", out.Raw)
	}
}

func TestBasicAuthEncodeMissingUser(t *testing.T) {
	meta := `{"secrets":{"password":{"secret":"P"}}}`
	ctx := withBagAndMeta(t, "P", []byte("token"), meta)
	_, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err == nil {
		t.Errorf("expected error for missing user")
	}
}

func TestBasicAuthEncodeMissingSecretRef(t *testing.T) {
	meta := `{"user":"foo"}`
	ctx := withBagAndMeta(t, "P", []byte("token"), meta)
	_, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err == nil {
		t.Errorf("expected error for missing secrets.password.secret")
	}
}

func TestBasicAuthEncodeMissingPasswordInBag(t *testing.T) {
	meta := `{"secrets":{"password":{"secret":"MISSING"}},"user":"foo"}`
	ctx := withBagAndMeta(t, "OTHER", []byte("token"), meta)
	_, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err == nil {
		t.Errorf("expected error for password name not in bag")
	}
}

func TestBasicAuthEncodeNoBag(t *testing.T) {
	meta := `{"secrets":{"password":{"secret":"P"}},"user":"foo"}`
	ctx := operation.WithMeta(context.Background(), meta) // no bag
	_, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err == nil {
		t.Errorf("expected error for no bag on ctx")
	}
}

// TestBasicAuthEncodeNoLeakInError is the load-bearing no-leak
// check: even on error paths, the response Meta must not contain
// the password cleartext.
func TestBasicAuthEncodeNoLeakInError(t *testing.T) {
	password := []byte("super-secret-twilio-token-xyz")
	// Trigger error: missing user.
	meta := `{"secrets":{"password":{"secret":"P"}}}`
	ctx := withBagAndMeta(t, "P", password, meta)
	out, err := BasicAuthEncode(ctx, "txco://basic-auth-encode", nil, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(out.Meta, string(password)) {
		t.Errorf("error meta leaks password: %s", out.Meta)
	}
	if strings.Contains(out.Raw, string(password)) {
		t.Errorf("error raw leaks password: %s", out.Raw)
	}
}
