package ops

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// withBagAndMeta builds a context carrying a SecretBag (with
// `name`→cleartext loaded) and the supplied op.Meta JSON.
func withBagAndMeta(t *testing.T, name string, cleartext []byte, meta string) context.Context {
	t.Helper()
	var bag secrets.SecretBag
	bag.Set(name, cleartext)
	ctx := secrets.WithBag(context.Background(), &bag)
	ctx = operation.WithMeta(ctx, meta)
	return ctx
}

func TestHMACSignSHA256Hex(t *testing.T) {
	// RFC 4231 test case 1 (HMAC-SHA256):
	//   key  = 0x0b * 20
	//   data = "Hi There"
	//   mac  = b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7
	//
	// We pass the data as the body of the input envelope so the
	// op's gjson path reads it.
	key := make([]byte, 20)
	for i := range key {
		key[i] = 0x0b
	}
	in := []byte(`{"body":"Hi There"}`)
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body"}`

	// A JSON string field is signed as its literal (unquoted) value, so the
	// digest matches the RFC 4231 vector exactly (the bug this guards against
	// was signing the quoted form `"Hi There"`, which matched nothing).
	const rfcVector = "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	ctx := withBagAndMeta(t, "K", key, meta)
	out1, err := HMACSign(ctx, "txco://hmac-sign", in, nil)
	if err != nil {
		t.Fatalf("HMACSign: %v", err)
	}
	sig1 := gjson.Get(out1.Raw, "_txc.computed.hmac").String()
	if sig1 != rfcVector {
		t.Fatalf("RFC 4231 vector mismatch:\n got  %s\n want %s", sig1, rfcVector)
	}

	// Determinism: same input twice → same digest.
	ctx2 := withBagAndMeta(t, "K", key, meta)
	out2, _ := HMACSign(ctx2, "txco://hmac-sign", in, nil)
	if gjson.Get(out2.Raw, "_txc.computed.hmac").String() != sig1 {
		t.Errorf("non-deterministic HMAC")
	}

	// Different input → different digest.
	in2 := []byte(`{"body":"Hi There!"}`)
	ctx3 := withBagAndMeta(t, "K", key, meta)
	out3, _ := HMACSign(ctx3, "txco://hmac-sign", in2, nil)
	if gjson.Get(out3.Raw, "_txc.computed.hmac").String() == sig1 {
		t.Errorf("HMAC collision on different inputs")
	}

	// Different key → different digest.
	key2 := make([]byte, 20)
	for i := range key2 {
		key2[i] = 0x0c
	}
	ctx4 := withBagAndMeta(t, "K", key2, meta)
	out4, _ := HMACSign(ctx4, "txco://hmac-sign", in, nil)
	if gjson.Get(out4.Raw, "_txc.computed.hmac").String() == sig1 {
		t.Errorf("HMAC collision on different keys")
	}
}

// TestHMACSignVerifyRoundTrip is the regression guard for the sign/verify
// string-input symmetry: a signature hmac-sign produces MUST verify with
// hmac-verify — for a string payload (the case that was broken) and a JSON
// object alike.
func TestHMACSignVerifyRoundTrip(t *testing.T) {
	key := []byte("round-trip-key")
	cases := map[string]string{
		"string": `{"payload":"white-fang.42.9f1c"}`,
		"object": `{"payload":{"a":1,"b":[2,3]}}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			signMeta := `{"secrets":{"key":{"secret":"K"}},"input_path":"payload","output_path":"sig"}`
			out, err := HMACSign(withBagAndMeta(t, "K", key, signMeta), "txco://hmac-sign", []byte(in), nil)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			sig := gjson.Get(out.Raw, "sig").String()

			// Carry the produced signature alongside the same input, then verify.
			vin, _ := sjson.Set(in, "sig", sig)
			vMeta := `{"secrets":{"key":{"secret":"K"}},"input_path":"payload","expected_path":"sig"}`
			vout, err := HMACVerify(withBagAndMeta(t, "K", key, vMeta), "txco://hmac-verify", []byte(vin), nil)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !gjson.Get(vout.Raw, "_txc.computed.sig_valid").Bool() {
				t.Fatalf("sign→verify did not round-trip for %s input; resp=%s", name, vout.Raw)
			}
		})
	}
}

func TestHMACSignAlgorithms(t *testing.T) {
	in := []byte(`{"body":"x"}`)
	key := []byte("secret-key")

	for _, alg := range []string{"sha256", "sha512"} {
		t.Run(alg, func(t *testing.T) {
			meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body","algorithm":"` + alg + `"}`
			ctx := withBagAndMeta(t, "K", key, meta)
			out, err := HMACSign(ctx, "txco://hmac-sign", in, nil)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			sig := gjson.Get(out.Raw, "_txc.computed.hmac").String()
			expectedLen := 64 // sha256
			if alg == "sha512" {
				expectedLen = 128
			}
			if len(sig) != expectedLen {
				t.Errorf("%s hex length = %d, want %d", alg, len(sig), expectedLen)
			}
		})
	}
}

func TestHMACSignBase64Encoding(t *testing.T) {
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body","encoding":"base64"}`
	ctx := withBagAndMeta(t, "K", []byte("key"), meta)
	out, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sig := gjson.Get(out.Raw, "_txc.computed.hmac").String()
	// base64 of 32 bytes = ~44 chars (44 with padding).
	if len(sig) != 44 {
		t.Errorf("base64 sig length = %d, want 44", len(sig))
	}
	if strings.Contains(sig, " ") || !strings.ContainsAny(sig, "+/=") && len(sig) < 40 {
		// Sanity check: must look like base64.
		t.Errorf("unexpected sig shape: %q", sig)
	}
}

func TestHMACSignCustomOutputPath(t *testing.T) {
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body","output_path":"_txc.computed.stripe_sig"}`
	ctx := withBagAndMeta(t, "K", []byte("key"), meta)
	out, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !gjson.Get(out.Raw, "_txc.computed.stripe_sig").Exists() {
		t.Errorf("custom output_path not honored: %s", out.Raw)
	}
}

func TestHMACSignMissingSecret(t *testing.T) {
	// secrets.key.secret refers to a name not in the bag.
	meta := `{"secrets":{"key":{"secret":"MISSING"}},"input_path":"body"}`
	ctx := withBagAndMeta(t, "OTHER", []byte("v"), meta)
	_, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err == nil {
		t.Errorf("expected error for missing secret name in bag")
	}
}

func TestHMACSignNoBag(t *testing.T) {
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body"}`
	ctx := operation.WithMeta(context.Background(), meta) // no bag
	_, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err == nil {
		t.Errorf("expected error for no bag on ctx")
	}
}

func TestHMACSignMissingSecretRef(t *testing.T) {
	// secrets.key.secret not declared at all.
	meta := `{"input_path":"body"}`
	ctx := withBagAndMeta(t, "K", []byte("v"), meta)
	_, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err == nil {
		t.Errorf("expected error for missing secrets.key.secret")
	}
}

func TestHMACSignUnsupportedAlgorithm(t *testing.T) {
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body","algorithm":"md5"}`
	ctx := withBagAndMeta(t, "K", []byte("v"), meta)
	_, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err == nil {
		t.Errorf("expected error for unsupported algorithm md5")
	}
}

func TestHMACSignUnsupportedEncoding(t *testing.T) {
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body","encoding":"base32"}`
	ctx := withBagAndMeta(t, "K", []byte("v"), meta)
	_, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"data"}`), nil)
	if err == nil {
		t.Errorf("expected error for unsupported encoding base32")
	}
}

// TestHMACSignOutputDoesNotLeakKey is the load-bearing no-leak test
// for the computed-secret pattern: the response payload contains
// only the digest, never any byte of the secret key.
func TestHMACSignOutputDoesNotLeakKey(t *testing.T) {
	key := []byte("super-secret-stripe-webhook-key-xyz-123")
	meta := `{"secrets":{"key":{"secret":"K"}},"input_path":"body"}`
	ctx := withBagAndMeta(t, "K", key, meta)
	out, err := HMACSign(ctx, "txco://hmac-sign", []byte(`{"body":"event-data"}`), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if strings.Contains(out.Raw, string(key)) {
		t.Errorf("response leaks key bytes: %s", out.Raw)
	}
	if strings.Contains(out.Meta, string(key)) {
		t.Errorf("response meta leaks key bytes: %s", out.Meta)
	}
}
