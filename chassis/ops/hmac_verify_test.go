package ops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func hexHMAC256(key []byte, data string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// verifyInput builds an input envelope with the signed string at
// `signed` and the expected signature at `v1`, JSON-encoded so the
// string survives gjson round-tripping.
func verifyInput(t *testing.T, signed, expected string) []byte {
	t.Helper()
	in := `{}`
	in, _ = sjson.Set(in, "signed", signed)
	in, _ = sjson.Set(in, "v1", expected)
	return []byte(in)
}

const verifyMeta = `{"secrets":{"key":{"secret":"WHSEC"}},"input_path":"signed","expected_path":"v1"}`

func sigValid(out string) bool { return gjson.Get(out, "_txc.computed.sig_valid").Bool() }

func TestHMACVerifyValid(t *testing.T) {
	key := []byte("whsec_test")
	// Stripe-style "t.body" literal — exercises the string-input path.
	signed := "1690000000." + `{"id":"evt_1"}`
	in := verifyInput(t, signed, hexHMAC256(key, signed))
	ctx := withBagAndMeta(t, "WHSEC", key, verifyMeta)

	out, err := HMACVerify(ctx, "txco://hmac-verify", in, nil)
	if err != nil {
		t.Fatalf("HMACVerify: %v", err)
	}
	if !sigValid(out.Raw) {
		t.Errorf("sig_valid = false, want true; resp=%s", out.Raw)
	}
}

func TestHMACVerifyTampered(t *testing.T) {
	key := []byte("whsec_test")
	signed := "1690000000.body"
	good := hexHMAC256(key, signed)
	bad := good[:len(good)-1] // drop, then append a different nibble
	if good[len(good)-1] == 'a' {
		bad += "b"
	} else {
		bad += "a"
	}
	ctx := withBagAndMeta(t, "WHSEC", key, verifyMeta)

	out, _ := HMACVerify(ctx, "txco://hmac-verify", verifyInput(t, signed, bad), nil)
	if sigValid(out.Raw) {
		t.Errorf("tampered signature accepted; resp=%s", out.Raw)
	}
}

func TestHMACVerifyWrongKey(t *testing.T) {
	signed := "1690000000.body"
	sig := hexHMAC256([]byte("real_key"), signed)
	// Bag holds a different key than the one that produced sig.
	ctx := withBagAndMeta(t, "WHSEC", []byte("wrong_key"), verifyMeta)

	out, _ := HMACVerify(ctx, "txco://hmac-verify", verifyInput(t, signed, sig), nil)
	if sigValid(out.Raw) {
		t.Errorf("wrong key verified true; resp=%s", out.Raw)
	}
}

func TestHMACVerifyMalformedExpectedFailsClosed(t *testing.T) {
	key := []byte("whsec_test")
	ctx := withBagAndMeta(t, "WHSEC", key, verifyMeta)

	out, err := HMACVerify(ctx, "txco://hmac-verify", verifyInput(t, "data", "not-hex-zz"), nil)
	if err != nil {
		t.Fatalf("malformed expected should not error: %v", err)
	}
	if sigValid(out.Raw) {
		t.Errorf("malformed expected verified true; resp=%s", out.Raw)
	}
}

func TestHMACVerifyBase64(t *testing.T) {
	key := []byte("whsec_test")
	signed := "payload"
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signed))
	sigB64 := base64.StdEncoding.EncodeToString(m.Sum(nil))

	meta := `{"secrets":{"key":{"secret":"WHSEC"}},"input_path":"signed","expected_path":"v1","encoding":"base64"}`
	ctx := withBagAndMeta(t, "WHSEC", key, meta)

	out, err := HMACVerify(ctx, "txco://hmac-verify", verifyInput(t, signed, sigB64), nil)
	if err != nil {
		t.Fatalf("HMACVerify base64: %v", err)
	}
	if !sigValid(out.Raw) {
		t.Errorf("base64 sig_valid = false, want true; resp=%s", out.Raw)
	}
}

func TestHMACVerifyMissingExpectedPathErrors(t *testing.T) {
	key := []byte("whsec_test")
	// expected_path omitted → config error (distinct from a failed verify).
	meta := `{"secrets":{"key":{"secret":"WHSEC"}},"input_path":"signed"}`
	ctx := withBagAndMeta(t, "WHSEC", key, meta)

	if _, err := HMACVerify(ctx, "txco://hmac-verify", verifyInput(t, "data", "abcd"), nil); err == nil {
		t.Error("expected error for missing expected_path")
	}
}
